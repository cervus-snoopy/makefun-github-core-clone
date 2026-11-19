// REST v3 part 2: contents (files), issues, pull requests.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// resolveRef turns a ref name (branch), a commit sha, or "" (default branch)
// into a commit oid.
func resolveRef(r *repo, ref string) (string, bool) {
	if ref == "" {
		return r.defaultTip()
	}
	if oid, ok := r.refTip("refs/heads/" + ref); ok {
		return oid, true
	}
	if o := r.getObject(ref); o != nil {
		if o.typ == objCommit {
			return ref, true
		}
		if o.typ == objTag {
			target, _, _, _, _ := parseTag(o.data)
			if to := r.getObject(target); to != nil && to.typ == objCommit {
				return target, true
			}
		}
	}
	return "", false
}

// dirEntries lists the entries of a directory inside a tree.
func dirEntries(r *repo, treeOID, dirPath string) ([]treeEntry, bool) {
	oid := treeOID
	if dirPath != "" {
		for _, part := range strings.Split(dirPath, "/") {
			o := r.getObject(oid)
			if o == nil || o.typ != objTree {
				return nil, false
			}
			found := false
			for _, e := range parseTree(o.data) {
				if e.name == part && e.mode == "40000" {
					oid = e.oid
					found = true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
	}
	o := r.getObject(oid)
	if o == nil || o.typ != objTree {
		return nil, false
	}
	return parseTree(o.data), true
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func contentsRoute(req *httpRequest, r *repo, sub []string) *httpResponse {
	path := strings.Join(sub, "/")
	switch req.method {
	case "GET":
		return contentsGet(req, r, path)
	case "PUT":
		return contentsPut(req, r, path)
	case "DELETE":
		return contentsDelete(req, r, path)
	}
	return jsonErr(404, "Not Found")
}

func contentsGet(req *httpRequest, r *repo, path string) *httpResponse {
	tip, ok := resolveRef(r, queryValue(req.query, "ref"))
	if !ok {
		return jsonErr(404, "Not Found")
	}
	treeOID := parseCommit(r.getObject(tip).data).tree
	if path == "" {
		entries, _ := dirEntries(r, treeOID, "")
		return jsonResp(200, dirListJSON(r, "", entries))
	}
	flat := flattenTree(r.getObject, treeOID)
	if leaf, ok := flat[path]; ok {
		o := r.getObject(leaf.oid)
		var sb strings.Builder
		sb.WriteString(`{"type":"file","encoding":"base64","size":`)
		sb.WriteString(strconv.Itoa(len(o.data)))
		sb.WriteString(`,"name":`)
		jsonEscape(&sb, baseName(path))
		sb.WriteString(`,"path":`)
		jsonEscape(&sb, path)
		sb.WriteString(`,"sha":`)
		jsonEscape(&sb, leaf.oid)
		sb.WriteString(`,"content":`)
		jsonEscape(&sb, base64.StdEncoding.EncodeToString(o.data))
		sb.WriteByte('}')
		return jsonResp(200, []byte(sb.String()))
	}
	if entries, ok := dirEntries(r, treeOID, path); ok {
		return jsonResp(200, dirListJSON(r, path, entries))
	}
	return jsonErr(404, "Not Found")
}

func dirListJSON(r *repo, dirPath string, entries []treeEntry) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		typ := "file"
		if e.mode == "40000" {
			typ = "dir"
		}
		p := e.name
		if dirPath != "" {
			p = dirPath + "/" + e.name
		}
		sb.WriteString(`{"type":`)
		jsonEscape(&sb, typ)
		sb.WriteString(`,"name":`)
		jsonEscape(&sb, e.name)
		sb.WriteString(`,"path":`)
		jsonEscape(&sb, p)
		sb.WriteString(`,"sha":`)
		jsonEscape(&sb, e.oid)
		if typ == "file" {
			if o := r.getObject(e.oid); o != nil {
				sb.WriteString(`,"size":`)
				sb.WriteString(strconv.Itoa(len(o.data)))
			}
		}
		sb.WriteByte('}')
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

func contentsPut(req *httpRequest, r *repo, path string) *httpResponse {
	body, errResp := parseBody(req)
	if errResp != nil {
		return errResp
	}
	if path == "" {
		return jsonErr(422, "path is required")
	}
	message := body.get("message").strOr("")
	contentB64 := body.get("content").strOr("")
	clean := strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			return -1
		}
		return c
	}, contentB64)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return jsonErr(400, "invalid base64 content")
	}
	branch := body.get("branch").strOr("main")
	if branch == "" {
		branch = "main"
	}
	shaParam := body.get("sha").strOr("")
	// Current state of the branch.
	tip, hasTip := r.refTip("refs/heads/" + branch)
	flat := make(map[string]treeLeaf)
	if hasTip {
		if o := r.getObject(tip); o != nil && o.typ == objCommit {
			flat = flattenTree(r.getObject, parseCommit(o.data).tree)
		}
	} else if branch != "main" {
		// Founding a new branch: base it on the default branch tip.
		if dt, ok := r.defaultTip(); ok {
			if o := r.getObject(dt); o != nil && o.typ == objCommit {
				tip = dt
				hasTip = true
				flat = flattenTree(r.getObject, parseCommit(o.data).tree)
			}
		}
	}
	existing, hasExisting := flat[path]
	if shaParam != "" && (!hasExisting || shaParam != existing.oid) {
		return jsonErr(409, "sha does not match")
	}
	blobOID := r.putObject(&gitObject{typ: objBlob, data: raw})
	flat[path] = treeLeaf{oid: blobOID, mode: "100644"}
	treeOID := buildTreeFromFlat(func(oid string, o *gitObject) { r.putObject(o) }, flat)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitOID := r.putObject(&gitObject{typ: objCommit, data: buildCommit(treeOID, parents, fixedWho, fixedWho, message)})
	r.setRef("refs/heads/"+branch, commitOID)
	var sb strings.Builder
	sb.WriteString(`{"content":{"name":`)
	jsonEscape(&sb, baseName(path))
	sb.WriteString(`,"path":`)
	jsonEscape(&sb, path)
	sb.WriteString(`,"sha":`)
	jsonEscape(&sb, blobOID)
	sb.WriteString(`,"size":`)
	sb.WriteString(strconv.Itoa(len(raw)))
	sb.WriteString(`,"encoding":"base64","type":"file"},"commit":{"sha":`)
	jsonEscape(&sb, commitOID)
	sb.WriteString(`,"message":`)
	jsonEscape(&sb, strings.TrimSuffix(message, "\n"))
	sb.WriteString(`}}`)
	status := 201
	if hasExisting {
		status = 200
	}
	return jsonResp(status, []byte(sb.String()))
}

func contentsDelete(req *httpRequest, r *repo, path string) *httpResponse {
	body, errResp := parseBody(req)
	if errResp != nil {
		return errResp
	}
	message := body.get("message").strOr("")
	branch := body.get("branch").strOr("main")
	if branch == "" {
		branch = "main"
	}
	tip, hasTip := r.refTip("refs/heads/" + branch)
	if !hasTip {
		return jsonErr(404, "Not Found")
	}
	flat := flattenTree(r.getObject, parseCommit(r.getObject(tip).data).tree)
	if _, ok := flat[path]; !ok {
		return jsonErr(404, "Not Found")
	}
	delete(flat, path)
	treeOID := buildTreeFromFlat(func(oid string, o *gitObject) { r.putObject(o) }, flat)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commitOID := r.putObject(&gitObject{typ: objCommit, data: buildCommit(treeOID, []string{tip}, fixedWho, fixedWho, message)})
	r.setRef("refs/heads/"+branch, commitOID)
	var sb strings.Builder
	sb.WriteString(`{"commit":{"sha":`)
	jsonEscape(&sb, commitOID)
	sb.WriteString(`}}`)
	return jsonResp(200, []byte(sb.String()))
}

// ---------------------------------------------------------------- issues

func (s *storeT) createIssue(r *repo, title, body string, labels []string) (*issue, string) {
	for _, ln := range labels {
		if _, ok := r.Labels[ln]; !ok {
			return nil, "Label '" + ln + "' does not exist"
		}
	}
	is := &issue{
		Number: r.NextNumber,
		ID:     s.nextID(),
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
	}
	r.NextNumber++
	r.Issues = append(r.Issues, is)
	r.bump()
	return is, ""
}

func issuesRoute(req *httpRequest, r *repo, sub []string) *httpResponse {
	if len(sub) == 0 {
		switch req.method {
		case "POST":
			return createIssueREST(req, r)
		case "GET":
			return listIssuesREST(req, r)
		}
		return jsonErr(404, "Not Found")
	}
	n, err := strconv.Atoi(sub[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 1 {
		switch req.method {
		case "GET":
			is := r.findIssue(n)
			if is == nil {
				return jsonErr(404, "Not Found")
			}
			return jsonResp(200, store.issueJSON(r, is))
		case "PATCH":
			return patchIssueREST(req, r, n)
		}
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 2 && sub[1] == "comments" {
		is := r.findIssue(n)
		if is == nil {
			return jsonErr(404, "Not Found")
		}
		switch req.method {
		case "GET":
			var sb strings.Builder
			sb.WriteByte('[')
			for i, c := range is.Comments {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`{"id":`)
				sb.WriteString(strconv.Itoa(c.ID))
				sb.WriteString(`,"body":`)
				jsonEscape(&sb, c.Body)
				sb.WriteString(`,"user":{"login":`)
				jsonEscape(&sb, viewerLogin)
				sb.WriteString(`,"id":`)
				sb.WriteString(strconv.Itoa(viewerID))
				sb.WriteString(`,"type":"User"},"created_at":`)
				jsonEscape(&sb, fixedISO)
				sb.WriteByte('}')
			}
			sb.WriteByte(']')
			return jsonResp(200, []byte(sb.String()))
		case "POST":
			body, errResp := parseBody(req)
			if errResp != nil {
				return errResp
			}
			c := &comment{ID: store.nextID(), Body: body.get("body").strOr("")}
			is.Comments = append(is.Comments, c)
			is.cached = nil
			var sb strings.Builder
			sb.WriteString(`{"id":`)
			sb.WriteString(strconv.Itoa(c.ID))
			sb.WriteString(`,"body":`)
			jsonEscape(&sb, c.Body)
			sb.WriteString(`,"user":{"login":`)
			jsonEscape(&sb, viewerLogin)
			sb.WriteString(`,"id":`)
			sb.WriteString(strconv.Itoa(viewerID))
			sb.WriteString(`,"type":"User"},"created_at":`)
			jsonEscape(&sb, fixedISO)
			sb.WriteByte('}')
			return jsonResp(201, []byte(sb.String()))
		}
	}
	return jsonErr(404, "Not Found")
}

func createIssueREST(req *httpRequest, r *repo) *httpResponse {
	body, errResp := parseBody(req)
	if errResp != nil {
		return errResp
	}
	title := body.get("title").strOr("")
	b := body.get("body").strOr("")
	var labels []string
	if lv := body.get("labels"); lv != nil && lv.kind == 'a' {
		for _, item := range lv.arr {
			if item.kind == 's' {
				labels = append(labels, item.str)
			} else if item.kind == 'o' {
				labels = append(labels, item.get("name").strOr(""))
			}
		}
	}
	is, errMsg := store.createIssue(r, title, b, labels)
	if errMsg != "" {
		return jsonErr(422, errMsg)
	}
	return jsonResp(201, store.issueJSON(r, is))
}

func listIssuesREST(req *httpRequest, r *repo) *httpResponse {
	state := queryValue(req.query, "state")
	if state == "" {
		state = "open"
	}
	dir := queryValue(req.query, "direction")
	if dir == "" {
		dir = "desc"
	}
	perPage, page := pageParams(req)
	var list []*issue
	for _, is := range r.Issues {
		if state == "all" || is.State == state {
			list = append(list, is)
		}
	}
	// Sort by creation: desc = newest first (number desc).
	if dir == "desc" {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	total := len(list)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			sb.WriteByte(',')
		}
		sb.Write(store.issueJSON(r, list[i]))
	}
	sb.WriteByte(']')
	return withLink(jsonResp(200, []byte(sb.String())), linkHeader(req, perPage, page, total))
}

func patchIssueREST(req *httpRequest, r *repo, n int) *httpResponse {
	is := r.findIssue(n)
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	body, errResp := parseBody(req)
	if errResp != nil {
		return errResp
	}
	if st := body.get("state"); st != nil && st.kind == 's' {
		if st.str == "open" || st.str == "closed" {
			is.State = st.str
		}
	}
	if t := body.get("title"); t != nil && t.kind == 's' {
		is.Title = t.str
	}
	if b := body.get("body"); b != nil && b.kind == 's' {
		is.Body = b.str
	}
	is.cached = nil
	return jsonResp(200, store.issueJSON(r, is))
}

// ---------------------------------------------------------------- pulls

func (s *storeT) createPR(r *repo, head, base, title, body string) (*pullRequest, string) {
	if _, ok := r.refTip("refs/heads/" + head); !ok {
		return nil, "head branch does not exist"
	}
	if _, ok := r.refTip("refs/heads/" + base); !ok {
		return nil, "base branch does not exist"
	}
	for _, pr := range r.PRs {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, "A pull request already exists for " + head
		}
	}
	pr := &pullRequest{
		Number:  r.NextNumber,
		ID:      s.nextID(),
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
	}
	r.NextNumber++
	r.PRs = append(r.PRs, pr)
	r.bump()
	return pr, ""
}

func pullsRoute(req *httpRequest, r *repo, sub []string) *httpResponse {
	if len(sub) == 0 {
		switch req.method {
		case "POST":
			body, errResp := parseBody(req)
			if errResp != nil {
				return errResp
			}
			head := body.get("head").strOr("")
			base := body.get("base").strOr("")
			title := body.get("title").strOr("")
			b := body.get("body").strOr("")
			pr, errMsg := store.createPR(r, head, base, title, b)
			if errMsg != "" {
				return jsonErr(422, errMsg)
			}
			return jsonResp(201, store.prJSON(r, pr))
		case "GET":
			state := queryValue(req.query, "state")
			if state == "" {
				state = "open"
			}
			perPage, page := pageParams(req)
			var list []*pullRequest
			for _, pr := range r.PRs {
				if state == "all" || pr.State == state {
					list = append(list, pr)
				}
			}
			total := len(list)
			start := (page - 1) * perPage
			if start > total {
				start = total
			}
			end := start + perPage
			if end > total {
				end = total
			}
			var sb strings.Builder
			sb.WriteByte('[')
			for i := start; i < end; i++ {
				if i > start {
					sb.WriteByte(',')
				}
				sb.Write(store.prJSON(r, list[i]))
			}
			sb.WriteByte(']')
			return withLink(jsonResp(200, []byte(sb.String())), linkHeader(req, perPage, page, total))
		}
		return jsonErr(404, "Not Found")
	}
	n, err := strconv.Atoi(sub[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	pr := r.findPR(n)
	if len(sub) == 1 {
		if pr == nil {
			return jsonErr(404, "Not Found")
		}
		if req.method == "GET" {
			return jsonResp(200, store.prJSON(r, pr))
		}
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 2 && sub[1] == "files" && req.method == "GET" {
		if pr == nil {
			return jsonErr(404, "Not Found")
		}
		headSHA, ok1 := r.refTip("refs/heads/" + pr.HeadRef)
		baseSHA, ok2 := r.refTip("refs/heads/" + pr.BaseRef)
		if !ok1 || !ok2 {
			return jsonErr(404, "Not Found")
		}
		mb := mergeBase(r.getObject, headSHA, baseSHA)
		baseTree := ""
		if mb != "" {
			if o := r.getObject(mb); o != nil && o.typ == objCommit {
				baseTree = parseCommit(o.data).tree
			}
		}
		if baseTree == "" {
			if o := r.getObject(baseSHA); o != nil && o.typ == objCommit {
				baseTree = parseCommit(o.data).tree
			}
		}
		headTree := ""
		if o := r.getObject(headSHA); o != nil && o.typ == objCommit {
			headTree = parseCommit(o.data).tree
		}
		diffs := diffTrees(r.getObject, baseTree, headTree)
		var sb strings.Builder
		sb.WriteByte('[')
		for i, d := range diffs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"filename":`)
			jsonEscape(&sb, d.path)
			sb.WriteString(`,"status":`)
			jsonEscape(&sb, d.status)
			sb.WriteString(`,"additions":`)
			sb.WriteString(strconv.Itoa(d.additions))
			sb.WriteString(`,"deletions":`)
			sb.WriteString(strconv.Itoa(d.deletions))
			sb.WriteString(`,"changes":`)
			sb.WriteString(strconv.Itoa(d.additions + d.deletions))
			sb.WriteByte('}')
		}
		sb.WriteByte(']')
		return jsonResp(200, []byte(sb.String()))
	}
	if len(sub) == 2 && sub[1] == "merge" && req.method == "PUT" {
		if pr == nil {
			return jsonErr(404, "Not Found")
		}
		if pr.Merged {
			var sb strings.Builder
			sb.WriteString(`{"sha":`)
			jsonEscape(&sb, pr.MergeSHA)
			sb.WriteString(`,"merged":true,"message":"Pull request successfully merged"}`)
			return jsonResp(200, []byte(sb.String()))
		}
		sha, conflict, ok := store.mergePR(r, pr)
		if !ok {
			return jsonErr(404, "Not Found")
		}
		if conflict {
			return jsonErr(405, "Merge conflict")
		}
		var sb strings.Builder
		sb.WriteString(`{"sha":`)
		jsonEscape(&sb, sha)
		sb.WriteString(`,"merged":true,"message":"Pull request successfully merged"}`)
		return jsonResp(200, []byte(sb.String()))
	}
	return jsonErr(404, "Not Found")
}
