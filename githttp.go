// Git smart HTTP: info/refs (v0 + v2), ls-refs, fetch (v2 + v0),
// receive-pack. Pack/fetch responses are cached per repo generation and
// invalidated on any ref/object change.
package main

import (
	"sort"
	"strconv"
	"strings"
)

const agentCap = "forge/1.0"

// fetchCached returns a cached response body if it matches the repo generation.
func (r *repo) fetchCached(key string) ([]byte, bool) {
	e, ok := r.FetchCache[key]
	if !ok || e.gen != r.Gen {
		return nil, false
	}
	return e.body, true
}

func (r *repo) fetchStore(key string, body []byte) {
	if len(r.FetchCache) >= 128 && len(r.FetchOrder) > 0 {
		oldest := r.FetchOrder[0]
		r.FetchOrder = r.FetchOrder[1:]
		delete(r.FetchCache, oldest)
	}
	if _, ok := r.FetchCache[key]; !ok {
		r.FetchOrder = append(r.FetchOrder, key)
	}
	r.FetchCache[key] = &fetchEntry{gen: r.Gen, body: body}
}

func (r *repo) advCached(key string) ([]byte, bool) {
	e, ok := r.AdvCache[key]
	if !ok || e.gen != r.Gen {
		return nil, false
	}
	return e.body, true
}

func (r *repo) advStore(key string, body []byte) {
	r.AdvCache[key] = &fetchEntry{gen: r.Gen, body: body}
}

// sortedRefNames returns ref names in deterministic (lexicographic) order.
func (r *repo) sortedRefNames() []string {
	names := append([]string(nil), r.RefOrder...)
	sort.Strings(names)
	return names
}

func (r *repo) headTip() (string, bool) {
	return r.refTip("refs/heads/main")
}

// ---------------------------------------------------------------- advertisements

func uploadPackV0Caps(r *repo) string {
	caps := "side-band-64k thin-pack ofs-delta multi_ack shallow agent=" + agentCap
	if _, ok := r.headTip(); ok {
		caps += " symref=HEAD:refs/heads/main"
	}
	return caps
}

func infoRefsV0(r *repo, service string) []byte {
	var out []byte
	out = append(out, pktText("# service="+service+"\n")...)
	out = append(out, pktFlush...)
	names := r.sortedRefNames()
	caps := ""
	if service == "git-upload-pack" {
		caps = uploadPackV0Caps(r)
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentCap
	}
	if len(names) == 0 {
		line := zeroOID + " capabilities^{}\x00" + caps + "\n"
		out = append(out, pktText(line)...)
	} else {
		for i, name := range names {
			oid := r.Refs[name]
			line := oid + " " + name
			if i == 0 {
				line += "\x00" + caps
			}
			line += "\n"
			out = append(out, pktText(line)...)
		}
	}
	out = append(out, pktFlush...)
	return out
}

func infoRefsV2(r *repo, service string) []byte {
	// The v2 advertisement is identical for both services in this forge.
	var out []byte
	out = append(out, pktText("version 2\n")...)
	out = append(out, pktText("agent="+agentCap+"\n")...)
	out = append(out, pktText("ls-refs=unborn\n")...)
	out = append(out, pktText("fetch=shallow filter\n")...)
	out = append(out, pktText("object-format=sha1\n")...)
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------- ls-refs

func lsRefs(r *repo, peel, symrefs, unborn bool, prefixes []string) []byte {
	var out []byte
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	tip, hasTip := r.headTip()
	headMatches := len(prefixes) == 0
	for _, p := range prefixes {
		if strings.HasPrefix("HEAD", p) {
			headMatches = true
		}
	}
	if headMatches {
		if hasTip {
			line := tip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			out = append(out, pktText(line+"\n")...)
		} else if unborn {
			out = append(out, pktText("unborn HEAD symref-target:refs/heads/main\n")...)
		}
	}
	for _, name := range r.sortedRefNames() {
		if !matches(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel {
			if o := r.getObject(oid); o != nil && o.typ == objTag {
				target, _, _, _, _ := parseTag(o.data)
				if to := r.getObject(target); to != nil && to.typ == objCommit {
					line += " peeled:" + target
				}
			}
		}
		out = append(out, pktText(line+"\n")...)
	}
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------- fetch

type fetchReq struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int
	done    bool
}

func parseFetchLines(lines []pkt) fetchReq {
	var fr fetchReq
	for _, p := range lines {
		if p.kind != 'd' {
			continue
		}
		s := strings.TrimRight(string(p.data), "\n")
		switch {
		case strings.HasPrefix(s, "want "):
			v := s[5:]
			if i := strings.IndexByte(v, 0); i >= 0 {
				v = v[:i]
			}
			if i := strings.IndexByte(v, ' '); i >= 0 {
				v = v[:i]
			}
			fr.wants = append(fr.wants, v)
		case strings.HasPrefix(s, "have "):
			fr.haves = append(fr.haves, s[5:])
		case strings.HasPrefix(s, "shallow "):
			fr.shallow = append(fr.shallow, s[8:])
		case strings.HasPrefix(s, "deepen "):
			if n, err := strconv.Atoi(s[7:]); err == nil {
				fr.depth = n
			}
		case s == "done":
			fr.done = true
		}
	}
	return fr
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func buildFetchResponse(r *repo, fr fetchReq, v2 bool) []byte {
	// Negotiation still open (no `done`): real git expects an
	// acknowledgments-only response, then it POSTs again with `done`.
	// Sending the packfile here makes git abort with
	// "expected response end packet".
	if v2 && !fr.done {
		var out []byte
		out = append(out, pktText("acknowledgments\n")...)
		out = append(out, pktText("NAK\n")...)
		out = append(out, pktFlush...)
		return out
	}
	plan := planFetch(r, fr.wants, fr.haves, fr.shallow, fr.depth)
	var objs []*gitObject
	for _, oid := range plan.send {
		if o := r.getObject(oid); o != nil {
			objs = append(objs, o)
		}
	}
	pack := writePack(objs)
	var out []byte
	if v2 {
		if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
			out = append(out, pktText("shallow-info\n")...)
			for _, oid := range sortedCopy(plan.shallow) {
				out = append(out, pktText("shallow "+oid+"\n")...)
			}
			for _, oid := range sortedCopy(plan.unshallow) {
				out = append(out, pktText("unshallow "+oid+"\n")...)
			}
			out = append(out, pktFlush...)
		}
		out = append(out, pktText("packfile\n")...)
		out = append(out, sidebandEncode(pack)...)
		out = append(out, pktFlush...)
		return out
	}
	// v0
	out = append(out, pktText("NAK\n")...)
	for _, oid := range sortedCopy(plan.shallow) {
		out = append(out, pktText("shallow "+oid+"\n")...)
	}
	for _, oid := range sortedCopy(plan.unshallow) {
		out = append(out, pktText("unshallow "+oid+"\n")...)
	}
	out = append(out, sidebandEncode(pack)...)
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------- receive-pack

func receivePack(r *repo, body []byte) []byte {
	// Parse command pkt-lines only up to (and including) the first flush;
	// the pack bytes follow the flush and are NOT pkt-lines.
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	packStart := -1
	i := 0
	for i < len(body) {
		if i+4 > len(body) {
			return nil
		}
		hdr := string(body[i : i+4])
		if hdr == "0000" {
			packStart = i + 4
			break
		}
		if hdr == "0001" || hdr == "0002" {
			i += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || i+int(n) > len(body) {
			return nil
		}
		payload := body[i+4 : i+int(n)]
		s := strings.TrimRight(string(payload), "\n")
		if j := strings.IndexByte(s, 0); j >= 0 {
			s = s[:j]
		}
		parts := strings.SplitN(s, " ", 3)
		if len(parts) == 3 {
			cmds = append(cmds, cmd{old: parts[0], new: parts[1], ref: parts[2]})
		}
		i += int(n)
	}
	if packStart < 0 {
		return nil
	}
	var report []byte
	packBytes := body[packStart:]
	var unpackErr string
	if len(packBytes) > 0 {
		objs, err := readPack(packBytes, r.getObject)
		if err != nil {
			unpackErr = err.Error()
		} else {
			for _, o := range objs {
				r.putObject(o)
			}
		}
	}
	if unpackErr != "" {
		report = append(report, pktText("unpack "+unpackErr+"\n")...)
	} else {
		report = append(report, pktText("unpack ok\n")...)
	}
	for _, c := range cmds {
		if unpackErr != "" {
			report = append(report, pktText("ng "+c.ref+" unpack error\n")...)
			continue
		}
		cur, exists := r.Refs[c.ref]
		switch {
		case c.new == zeroOID: // delete
			if !exists {
				report = append(report, pktText("ng "+c.ref+" cannot lock ref\n")...)
			} else if c.old != zeroOID && c.old != cur {
				report = append(report, pktText("ng "+c.ref+" cannot lock ref\n")...)
			} else {
				r.deleteRef(c.ref)
				report = append(report, pktText("ok "+c.ref+"\n")...)
			}
		case c.old == zeroOID: // create
			if exists {
				report = append(report, pktText("ng "+c.ref+" already exists\n")...)
			} else if r.getObject(c.new) == nil {
				report = append(report, pktText("ng "+c.ref+" object not found\n")...)
			} else {
				r.setRef(c.ref, c.new)
				report = append(report, pktText("ok "+c.ref+"\n")...)
			}
		default: // update
			if !exists || c.old != cur {
				report = append(report, pktText("ng "+c.ref+" cannot lock ref\n")...)
			} else if r.getObject(c.new) == nil {
				report = append(report, pktText("ng "+c.ref+" object not found\n")...)
			} else if !isAncestor(r.getObject, c.old, c.new) {
				report = append(report, pktText("ng "+c.ref+" non-fast-forward\n")...)
			} else {
				r.setRef(c.ref, c.new)
				report = append(report, pktText("ok "+c.ref+"\n")...)
			}
		}
	}
	report = append(report, pktFlush...)
	out := sidebandEncode(report)
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------- git routing

// gitRoute handles /{owner}/{repo}.git/... ; returns nil when not a git path.
func gitRoute(req *httpRequest) *httpResponse {
	parts := strings.Split(strings.Trim(req.path, "/"), "/")
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		return nil
	}
	owner, repoName := parts[0], strings.TrimSuffix(parts[1], ".git")
	rest := strings.Join(parts[2:], "/")
	r := store.findRepo(owner, repoName)
	if r == nil {
		return &httpResponse{status: 404, ctype: "application/json", body: []byte(`{"message":"Not Found"}`)}
	}
	switch rest {
	case "info/refs":
		service := queryValue(req.query, "service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return &httpResponse{status: 400, ctype: "application/json", body: []byte(`{"message":"unknown service"}`)}
		}
		v2 := req.headers["git-protocol"] == "version=2"
		key := service + "|v2"
		if !v2 {
			key = service + "|v0"
		}
		if b, ok := r.advCached(key); ok {
			return &httpResponse{status: 200, ctype: "application/x-" + service + "-advertisement", body: b}
		}
		var b []byte
		if v2 {
			b = infoRefsV2(r, service)
		} else {
			b = infoRefsV0(r, service)
		}
		r.advStore(key, b)
		return &httpResponse{status: 200, ctype: "application/x-" + service + "-advertisement", body: b}
	case "git-upload-pack":
		if req.method != "POST" {
			return &httpResponse{status: 405, ctype: "application/json", body: []byte(`{"message":"Method Not Allowed"}`)}
		}
		lines, ok := readPktLines(req.body)
		if !ok || len(lines) == 0 {
			return &httpResponse{status: 400, ctype: "application/json", body: []byte(`{"message":"malformed pkt-line stream"}`)}
		}
		cmd := ""
		for _, p := range lines {
			if p.kind == 'd' {
				s := strings.TrimRight(string(p.data), "\n")
				if strings.HasPrefix(s, "command=") {
					cmd = s[8:]
				}
				break
			}
		}
		if cmd == "ls-refs" {
			peel, symrefs, unborn := false, false, false
			var prefixes []string
			for _, p := range lines {
				if p.kind != 'd' {
					continue
				}
				s := strings.TrimRight(string(p.data), "\n")
				switch {
				case s == "peel":
					peel = true
				case s == "symrefs":
					symrefs = true
				case s == "unborn":
					unborn = true
				case strings.HasPrefix(s, "ref-prefix "):
					prefixes = append(prefixes, s[11:])
				}
			}
			key := "ls|" + strconv.FormatBool(peel) + "|" + strconv.FormatBool(symrefs) + "|" + strconv.FormatBool(unborn) + "|" + strings.Join(prefixes, ",")
			if b, ok := r.advCached(key); ok {
				return &httpResponse{status: 200, ctype: "application/x-git-upload-pack-result", body: b}
			}
			b := lsRefs(r, peel, symrefs, unborn, prefixes)
			r.advStore(key, b)
			return &httpResponse{status: 200, ctype: "application/x-git-upload-pack-result", body: b}
		}
		if cmd != "fetch" {
			return &httpResponse{status: 400, ctype: "application/json", body: []byte(`{"message":"unknown command"}`)}
		}
		fr := parseFetchLines(lines)
		key := "f2|" + strings.Join(fr.wants, ",") + "|" + strings.Join(fr.haves, ",") + "|" + strings.Join(fr.shallow, ",") + "|" + strconv.Itoa(fr.depth) + "|" + strconv.FormatBool(fr.done)
		if b, ok := r.fetchCached(key); ok {
			return &httpResponse{status: 200, ctype: "application/x-git-upload-pack-result", body: b}
		}
		b := buildFetchResponse(r, fr, true)
		r.fetchStore(key, b)
		return &httpResponse{status: 200, ctype: "application/x-git-upload-pack-result", body: b}
	case "git-receive-pack":
		if req.method != "POST" {
			return &httpResponse{status: 405, ctype: "application/json", body: []byte(`{"message":"Method Not Allowed"}`)}
		}
		// receive-pack mutates the repo; dispatch holds the request guard.
		b := receivePack(r, req.body)
		if b == nil {
			return &httpResponse{status: 400, ctype: "application/json", body: []byte(`{"message":"malformed pkt-line stream"}`)}
		}
		return &httpResponse{status: 200, ctype: "application/x-git-receive-pack-result", body: b}
	}
	return nil
}
