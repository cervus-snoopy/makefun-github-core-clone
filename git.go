package main

// Git smart HTTP: info/refs advertisements (v0/v2), protocol v2
// ls-refs and fetch, and receive-pack (push).

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const agentCapsUpload = "side-band-64k thin-pack ofs-delta multi_ack shallow agent=forge/1.0"
const agentCapsReceive = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

// sortedStrings returns a sorted copy of the input.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// advertiseV0 builds a v0 advertisement for the given service.
func (r *Repo) advertiseV0(service string) []byte {
	caps := agentCapsUpload
	if service == "git-receive-pack" {
		caps = agentCapsReceive
	}
	if service == "git-upload-pack" {
		if _, ok := r.Refs["refs/heads/main"]; ok {
			caps += " symref=HEAD:refs/heads/main"
		}
	}
	var out []byte
	out = append(out, pktLine("# service="+service+"\n")...)
	out = append(out, pktFlush...)
	names := r.sortedRefNames()
	if len(names) == 0 {
		out = append(out, pktLine(zeroOID+" capabilities^{}\x00"+caps+"\n")...)
	} else {
		for i, name := range names {
			line := r.Refs[name] + " " + name
			if i == 0 {
				line += "\x00" + caps
			}
			line += "\n"
			out = append(out, pktLine(line)...)
		}
	}
	out = append(out, pktFlush...)
	return out
}

// advertiseV2 builds the protocol v2 advertisement (identical
// for both services).
func advertiseV2() []byte {
	var out []byte
	out = append(out, pktLine("version 2\n")...)
	out = append(out, pktLine("agent=forge/1.0\n")...)
	out = append(out, pktLine("ls-refs=unborn\n")...)
	out = append(out, pktLine("fetch=shallow filter\n")...)
	out = append(out, pktLine("object-format=sha1\n")...)
	out = append(out, pktFlush...)
	return out
}

// handleInfoRefs serves GET /{owner}/{repo}.git/info/refs.
func (r *Repo) handleInfoRefs(req *request) response {
	service := req.query("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return jsonErr(400, "unknown service")
	}
	v2 := req.header("git-protocol") == "version=2"
	key := service
	if v2 {
		key += "|v2"
	} else {
		key += "|v0"
	}
	if ent, ok := r.advCache[key]; ok && ent.gen == r.Gen {
		return response{status: 200, ctype: "application/x-" + service + "-advertisement", body: ent.data}
	}
	var data []byte
	if v2 {
		data = advertiseV2()
	} else {
		data = r.advertiseV0(service)
	}
	r.advCache[key] = cacheEntry{gen: r.Gen, data: data}
	return response{status: 200, ctype: "application/x-" + service + "-advertisement", body: data}
}

// lsRefs serves the protocol v2 ls-refs command.
func (r *Repo) lsRefs(packets []pktPacket) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, p := range packets {
		if p.Kind != "data" {
			continue
		}
		arg := strings.TrimSuffix(string(p.Data), "\n")
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	key := "ls|" + strconv.FormatBool(peel) + "|" + strconv.FormatBool(symrefs) + "|" +
		strconv.FormatBool(unborn) + "|" + strings.Join(prefixes, ",")
	if ent, ok := r.advCache[key]; ok && ent.gen == r.Gen {
		return ent.data
	}
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
	headOK := len(prefixes) == 0
	for _, p := range prefixes {
		if strings.HasPrefix("HEAD", p) {
			headOK = true
		}
	}
	if headOK {
		if tip, ok := r.Refs["refs/heads/main"]; ok {
			line := tip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			out = append(out, pktLine(line+"\n")...)
		} else if unborn {
			out = append(out, pktLine("unborn HEAD symref-target:refs/heads/main\n")...)
		}
	}
	for _, name := range r.sortedRefNames() {
		if !matches(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel {
			if o := r.getObject(oid); o != nil && o.Type == "tag" {
				if peeled := r.peelToCommit(oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		out = append(out, pktLine(line+"\n")...)
	}
	out = append(out, pktFlush...)
	r.advCache[key] = cacheEntry{gen: r.Gen, data: out}
	return out
}

// fetchPlan is the result of planning a fetch.
type fetchPlan struct {
	send      map[string]bool
	shallow   []string
	unshallow []string
}

// haveClosure walks from the haves, but never follows the parents
// of a commit in the client's shallow list.
func (r *Repo) haveClosure(haves, clientShallow []string) map[string]bool {
	shallowSet := map[string]bool{}
	for _, s := range clientShallow {
		shallowSet[s] = true
	}
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, haves...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		o := r.getObject(oid)
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.Type {
		case "commit":
			ci := parseCommit(o.Data)
			stack = append(stack, ci.Tree)
			if !shallowSet[oid] {
				stack = append(stack, ci.Parents...)
			}
		case "tree":
			for _, e := range parseTree(o.Data) {
				stack = append(stack, e.OID)
			}
		case "tag":
			stack = append(stack, parseTag(o.Data).Object)
		}
	}
	return seen
}

// generationsTo returns the number of generations from `from`
// down to `target` (0 when equal), or -1 when target is not an
// ancestor of from.
func (r *Repo) generationsTo(from, target string) int {
	from = r.peelToCommit(from)
	if from == "" {
		return -1
	}
	if from == target {
		return 0
	}
	type node struct {
		oid   string
		depth int
	}
	seen := map[string]bool{}
	queue := []node{{from, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.oid] {
			continue
		}
		seen[cur.oid] = true
		o := r.getObject(cur.oid)
		if o == nil || o.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(o.Data).Parents {
			pc := r.peelToCommit(p)
			if pc == "" {
				continue
			}
			if pc == target {
				return cur.depth + 1
			}
			queue = append(queue, node{pc, cur.depth + 1})
		}
	}
	return -1
}

// relativeDepth resolves a deepen-relative request: the new
// absolute depth is the deepest client-shallow boundary's depth
// measured from the wants, plus the relative amount. A boundary
// at distance k below a want sits at depth k+1 in the client's
// repo, so the client's current depth is max(k+1) over the
// boundaries that are ancestors of the wants.
func (r *Repo) relativeDepth(wants, clientShallow []string, extra int) int {
	base := 0
	for _, w := range wants {
		for _, s := range clientShallow {
			if d := r.generationsTo(w, s); d >= 0 && d+1 > base {
				base = d + 1
			}
		}
	}
	return base + extra
}

// planFetch computes which objects a fetch must send.
func (r *Repo) planFetch(wants, haves, clientShallow []string, depth int) fetchPlan {
	haveSet := r.haveClosure(haves, clientShallow)
	plan := fetchPlan{send: map[string]bool{}}
	if depth == 0 {
		reach := r.collectReachable(wants)
		for oid := range reach {
			if !haveSet[oid] {
				plan.send[oid] = true
			}
		}
		plan.unshallow = append([]string(nil), clientShallow...)
		return plan
	}
	// Depth-limited: BFS from the wants, tracking generations.
	type node struct {
		oid   string
		depth int
	}
	seenCommit := map[string]bool{}
	var frontier []string
	included := map[string]bool{}
	var queue []node
	for _, w := range wants {
		t := r.peelToCommit(w)
		if t != "" {
			queue = append(queue, node{t, 0})
		}
		if o := r.getObject(w); o != nil && o.Type == "tag" {
			plan.send[w] = true
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= depth || seenCommit[cur.oid] {
			continue
		}
		seenCommit[cur.oid] = true
		included[cur.oid] = true
		if cur.depth == depth-1 {
			frontier = append(frontier, cur.oid)
		}
		o := r.getObject(cur.oid)
		if o == nil || o.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(o.Data).Parents {
			pc := r.peelToCommit(p)
			if pc != "" {
				queue = append(queue, node{pc, cur.depth + 1})
			}
		}
	}
	// Send set: included commits plus their full tree closures
	// only. The parents of included commits are deliberately
	// NOT followed: a depth-limited fetch must not carry history
	// beyond the boundary (collectReachable would leak it).
	for oid := range included {
		plan.send[oid] = true
		if o := r.getObject(oid); o != nil && o.Type == "commit" {
			for toid := range r.treeClosure(parseCommit(o.Data).Tree) {
				plan.send[toid] = true
			}
		}
	}
	// Tag wants must be shipped even though they are not commits.
	for _, w := range wants {
		if o := r.getObject(w); o != nil && o.Type == "tag" {
			plan.send[w] = true
			target := parseTag(o.Data).Object
			if to := r.getObject(target); to != nil {
				switch to.Type {
				case "tree":
					for toid := range r.treeClosure(target) {
						plan.send[toid] = true
					}
				case "blob":
					plan.send[target] = true
				}
			}
		}
	}
	// Subtract what the client already holds.
	for oid := range plan.send {
		if haveSet[oid] {
			delete(plan.send, oid)
		}
	}
	plan.shallow = sortedStrings(frontier)
	frontierSet := map[string]bool{}
	for _, f := range frontier {
		frontierSet[f] = true
	}
	for _, s := range clientShallow {
		if included[s] && !frontierSet[s] {
			plan.unshallow = append(plan.unshallow, s)
		}
	}
	plan.shallow = sortedStrings(plan.shallow)
	plan.unshallow = sortedStrings(plan.unshallow)
	return plan
}

// buildFetchResponse builds the v2 fetch response bytes for a
// completed (done) fetch.
func (r *Repo) buildFetchResponse(plan fetchPlan) []byte {
	var out []byte
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out = append(out, pktLine("shallow-info\n")...)
		for _, s := range plan.shallow {
			out = append(out, pktLine("shallow "+s+"\n")...)
		}
		for _, s := range plan.unshallow {
			out = append(out, pktLine("unshallow "+s+"\n")...)
		}
		out = append(out, pktFlush...)
	}
	out = append(out, pktLine("packfile\n")...)
	var objs []*Obj
	for oid := range plan.send {
		if o := r.getObject(oid); o != nil {
			objs = append(objs, o)
		}
	}
	pack := writePack(objs)
	out = append(out, sidebandEncode(pack)...)
	out = append(out, pktFlush...)
	return out
}

// handleUploadPack serves POST /{owner}/{repo}.git/git-upload-pack.
func (r *Repo) handleUploadPack(req *request) response {
	packets, err := readPktLines(req.body)
	if err != nil || len(packets) == 0 {
		return jsonErr(400, "malformed pkt-line stream")
	}
	first := packets[0]
	if first.Kind != "data" {
		return jsonErr(400, "unknown command")
	}
	cmd := strings.TrimSuffix(string(first.Data), "\n")
	if !strings.HasPrefix(cmd, "command=") {
		return jsonErr(400, "unknown command")
	}
	command := cmd[len("command="):]
	switch command {
	case "ls-refs":
		return response{status: 200, ctype: "application/x-git-upload-pack-result", body: r.lsRefs(packets)}
	case "fetch":
		// fall through below
	default:
		return jsonErr(400, "unknown command")
	}
	// Parse fetch arguments.
	var wants, haves, shallow []string
	depth := 0
	deepenRelative := false
	done := false
	for _, p := range packets {
		if p.Kind != "data" {
			continue
		}
		arg := strings.TrimSuffix(string(p.Data), "\n")
		switch {
		case strings.HasPrefix(arg, "want "):
			w := arg[5:]
			if i := strings.IndexByte(w, 0); i >= 0 {
				w = w[:i]
			}
			if i := strings.IndexByte(w, ' '); i >= 0 {
				w = w[:i]
			}
			wants = append(wants, w)
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, arg[5:])
		case strings.HasPrefix(arg, "shallow "):
			shallow = append(shallow, arg[8:])
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(arg[7:]); err == nil {
				depth = n
			}
		case arg == "deepen-relative":
			deepenRelative = true
		case arg == "done":
			done = true
		}
	}
	// Real git sends deepen-relative: the deepen value counts
	// from the client's current depth, not from the tip.
	if deepenRelative && depth > 0 {
		depth = r.relativeDepth(wants, shallow, depth)
	}
	if !done {
		var out []byte
		out = append(out, pktLine("acknowledgments\n")...)
		out = append(out, pktLine("NAK\n")...)
		out = append(out, pktFlush...)
		return response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
	}
	key := "f2|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" +
		strings.Join(shallow, ",") + "|" + strconv.Itoa(depth) + "|true"
	if ent, ok := r.fetchCache[key]; ok && ent.gen == r.Gen {
		return response{status: 200, ctype: "application/x-git-upload-pack-result", body: ent.data}
	}
	plan := r.planFetch(wants, haves, shallow, depth)
	data := r.buildFetchResponse(plan)
	if len(r.fetchCache) >= 128 {
		oldest := r.fetchOrder[0]
		r.fetchOrder = r.fetchOrder[1:]
		delete(r.fetchCache, oldest)
	}
	r.fetchCache[key] = cacheEntry{gen: r.Gen, data: data}
	r.fetchOrder = append(r.fetchOrder, key)
	return response{status: 200, ctype: "application/x-git-upload-pack-result", body: data}
}

// handleReceivePack serves POST /{owner}/{repo}.git/git-receive-pack.
func (r *Repo) handleReceivePack(req *request) response {
	body := req.body
	// Parse pkt-lines up to and including the first flush; the
	// rest is the raw pack.
	flushEnd := -1
	pos := 0
	var commands [][3]string
	for pos < len(body) {
		if pos+4 > len(body) {
			return jsonErr(400, "malformed pkt-line stream")
		}
		hdr := string(body[pos : pos+4])
		if hdr == pktFlush {
			flushEnd = pos + 4
			break
		}
		if hdr == pktDelim || hdr == pktEnd {
			pos += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || pos+int(n) > len(body) {
			return jsonErr(400, "malformed pkt-line stream")
		}
		data := body[pos+4 : pos+int(n)]
		if i := bytes.IndexByte(data, 0); i >= 0 {
			data = data[:i]
		}
		line := strings.TrimSuffix(string(data), "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			commands = append(commands, [3]string{parts[0], parts[1], parts[2]})
		}
		pos += int(n)
	}
	if flushEnd < 0 {
		return jsonErr(400, "malformed pkt-line stream")
	}
	packBytes := body[flushEnd:]
	unpackErr := ""
	if len(packBytes) > 0 {
		objs, err := readPack(packBytes, func(oid string) *Obj { return r.getObject(oid) })
		if err != nil {
			unpackErr = err.Error()
		} else {
			for _, o := range objs {
				r.putObject(o.Type, o.Data)
			}
		}
	}
	// Build the report stream: each line is itself a pkt-line,
	// then an inner flush; the whole stream is sideband-encoded
	// and a final flush follows.
	var raw []byte
	if unpackErr != "" {
		raw = append(raw, pktLine("unpack "+unpackErr+"\n")...)
	} else {
		raw = append(raw, pktLine("unpack ok\n")...)
	}
	for _, cmd := range commands {
		oldOID, newOID, ref := cmd[0], cmd[1], cmd[2]
		if unpackErr != "" {
			raw = append(raw, pktLine("ng "+ref+" unpack error\n")...)
			continue
		}
		switch {
		case newOID == zeroOID: // delete
			cur, ok := r.Refs[ref]
			if !ok || (oldOID != zeroOID && oldOID != cur) {
				raw = append(raw, pktLine("ng "+ref+" cannot lock ref\n")...)
			} else {
				r.deleteRef(ref)
				raw = append(raw, pktLine("ok "+ref+"\n")...)
			}
		case oldOID == zeroOID: // create
			if _, ok := r.Refs[ref]; ok {
				raw = append(raw, pktLine("ng "+ref+" already exists\n")...)
			} else if r.getObject(newOID) == nil {
				raw = append(raw, pktLine("ng "+ref+" object not found\n")...)
			} else {
				r.setRef(ref, newOID)
				raw = append(raw, pktLine("ok "+ref+"\n")...)
			}
		default: // update
			cur, ok := r.Refs[ref]
			if !ok || oldOID != cur {
				raw = append(raw, pktLine("ng "+ref+" cannot lock ref\n")...)
			} else if r.getObject(newOID) == nil {
				raw = append(raw, pktLine("ng "+ref+" object not found\n")...)
			} else if !r.isAncestor(oldOID, newOID) {
				raw = append(raw, pktLine("ng "+ref+" non-fast-forward\n")...)
			} else {
				r.setRef(ref, newOID)
				raw = append(raw, pktLine("ok "+ref+"\n")...)
			}
		}
	}
	raw = append(raw, pktFlush...)
	encoded := sidebandEncode(raw)
	encoded = append(encoded, pktFlush...)
	return response{status: 200, ctype: "application/x-git-receive-pack-result", body: encoded}
}

// routeGit handles paths shaped /{owner}/{repo}.git/...; it
// returns (handled, response). Unrecognised git-shaped paths fall
// through to the generic 404.
func routeGit(req *request, store *Store) (bool, response) {
	trimmed := strings.Trim(req.path, "/")
	segs := strings.Split(trimmed, "/")
	if len(segs) < 3 || !strings.HasSuffix(segs[1], ".git") {
		return false, response{}
	}
	repoName := strings.TrimSuffix(segs[1], ".git")
	repo := store.getRepo(segs[0], repoName)
	if repo == nil {
		return true, jsonErr(404, "Not Found")
	}
	rest := strings.Join(segs[2:], "/")
	switch rest {
	case "info/refs":
		return true, repo.handleInfoRefs(req)
	case "git-upload-pack":
		if req.method != "POST" {
			return true, jsonErr(405, "Method Not Allowed")
		}
		return true, repo.handleUploadPack(req)
	case "git-receive-pack":
		if req.method != "POST" {
			return true, jsonErr(405, "Method Not Allowed")
		}
		return true, repo.handleReceivePack(req)
	}
	return false, response{}
}
