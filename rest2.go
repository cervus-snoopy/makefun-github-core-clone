package main

// REST API v3 (part two): contents, issues + comments, pull
// requests.

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ---- contents ----

// contentsEndpoint serves GET/PUT/DELETE .../contents/{path...}.
func contentsEndpoint(req *request, store *Store, repo *Repo, sub []string) response {
	path := strings.Join(sub, "/")
	switch req.method {
	case "GET":
		return contentsGet(req, repo, path)
	case "PUT":
		return contentsPut(req, store, repo, path)
	case "DELETE":
		return contentsDelete(req, store, repo, path)
	}
	return jsonErr(404, "Not Found")
}

// dirEntries lists the immediate children of a directory in a
// tree, in stored (git-sorted) order.
func (r *Repo) dirEntries(treeOID, dir string) ([]string, bool) {
	if dir == "" {
		o := r.getObject(treeOID)
		if o == nil || o.Type != "tree" {
			return nil, false
		}
		var out []string
		for _, e := range parseTree(o.Data) {
			out = append(out, dirEntryJSON(r, e, e.Name))
		}
		return out, true
	}
	// Walk down the tree by path components.
	cur := treeOID
	parts := strings.Split(dir, "/")
	for _, part := range parts {
		o := r.getObject(cur)
		if o == nil || o.Type != "tree" {
			return nil, false
		}
		found := ""
		for _, e := range parseTree(o.Data) {
			if e.Name == part && e.Mode == "40000" {
				found = e.OID
				break
			}
		}
		if found == "" {
			return nil, false
		}
		cur = found
	}
	o := r.getObject(cur)
	if o == nil || o.Type != "tree" {
		return nil, false
	}
	var out []string
	for _, e := range parseTree(o.Data) {
		out = append(out, dirEntryJSON(r, e, dir+"/"+e.Name))
	}
	return out, true
}

func dirEntryJSON(r *Repo, e TreeEntry, fullPath string) string {
	typ := "file"
	if e.Mode == "40000" {
		typ = "dir"
	}
	var b strings.Builder
	b.WriteString(`{"type":`)
	b.WriteString(jstr(typ))
	b.WriteString(`,"name":`)
	b.WriteString(jstr(e.Name))
	b.WriteString(`,"path":`)
	b.WriteString(jstr(fullPath))
	b.WriteString(`,"sha":`)
	b.WriteString(jstr(e.OID))
	if typ == "file" {
		if o := r.getObject(e.OID); o != nil && o.Type == "blob" {
			b.WriteString(`,"size":`)
			b.WriteString(strconv.Itoa(len(o.Data)))
		}
	}
	b.WriteString(`}`)
	return b.String()
}

func contentsGet(req *request, repo *Repo, path string) response {
	tip := repo.resolveRef(req.query("ref"))
	if tip == "" {
		return jsonErr(404, "Not Found")
	}
	treeOID := repo.treeOfCommit(tip)
	flat := repo.flattenTree(treeOID)
	if fe, ok := flat[path]; ok {
		data := repo.blobBytes(fe.OID)
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		var b strings.Builder
		b.WriteString(`{"type":"file","encoding":"base64","size":`)
		b.WriteString(strconv.Itoa(len(data)))
		b.WriteString(`,"name":`)
		b.WriteString(jstr(base))
		b.WriteString(`,"path":`)
		b.WriteString(jstr(path))
		b.WriteString(`,"sha":`)
		b.WriteString(jstr(fe.OID))
		b.WriteString(`,"content":`)
		b.WriteString(jstr(base64.StdEncoding.EncodeToString(data)))
		b.WriteString(`}`)
		return jsonOK(b.String())
	}
	if entries, ok := repo.dirEntries(treeOID, path); ok {
		return jsonOK("[" + strings.Join(entries, ",") + "]")
	}
	return jsonErr(404, "Not Found")
}

func contentsPut(req *request, store *Store, repo *Repo, path string) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	if path == "" {
		return jsonErr(422, "path is required")
	}
	content, err := decodeB64(body.StrField("content", ""))
	if err != nil {
		return jsonErr(400, "invalid base64 content")
	}
	message := body.StrField("message", "")
	branch := body.StrField("branch", "main")
	if branch == "" {
		branch = "main"
	}
	sha := body.StrField("sha", "")
	// Determine the parent commit and starting tree.
	var parent string
	var flat map[string]FileEntry
	if tip, ok := repo.Refs["refs/heads/"+branch]; ok {
		parent = tip
		flat = repo.flattenTree(repo.treeOfCommit(tip))
	} else if branch != "main" {
		if mainTip, ok := repo.Refs["refs/heads/main"]; ok {
			parent = mainTip
			flat = repo.flattenTree(repo.treeOfCommit(mainTip))
		} else {
			flat = map[string]FileEntry{}
		}
	} else {
		flat = map[string]FileEntry{}
	}
	existed := false
	if fe, ok := flat[path]; ok {
		existed = true
		if sha != "" && fe.OID != sha {
			return jsonErr(409, "sha does not match")
		}
	} else if sha != "" {
		return jsonErr(409, "sha does not match")
	}
	blobOID := repo.putObject("blob", content)
	flat[path] = FileEntry{OID: blobOID, Mode: "100644"}
	treeOID := repo.buildTreeFromFlat(flat)
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commitOID := repo.putObject("commit", buildCommit(treeOID, parents, msg))
	repo.setRef("refs/heads/"+branch, commitOID)
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	var b strings.Builder
	b.WriteString(`{"content":{"name":`)
	b.WriteString(jstr(base))
	b.WriteString(`,"path":`)
	b.WriteString(jstr(path))
	b.WriteString(`,"sha":`)
	b.WriteString(jstr(blobOID))
	b.WriteString(`,"size":`)
	b.WriteString(strconv.Itoa(len(content)))
	b.WriteString(`,"encoding":"base64","type":"file"},"commit":{"sha":`)
	b.WriteString(jstr(commitOID))
	b.WriteString(`,"message":`)
	b.WriteString(jstr(strings.TrimSuffix(msg, "\n")))
	b.WriteString(`}}`)
	status := 201
	if existed {
		status = 200
	}
	return response{status: status, ctype: "application/json; charset=utf-8", body: []byte(b.String())}
}

func contentsDelete(req *request, store *Store, repo *Repo, path string) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	message := body.StrField("message", "")
	branch := body.StrField("branch", "main")
	if branch == "" {
		branch = "main"
	}
	tip, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	flat := repo.flattenTree(repo.treeOfCommit(tip))
	if _, ok := flat[path]; !ok {
		return jsonErr(404, "Not Found")
	}
	delete(flat, path)
	treeOID := repo.buildTreeFromFlat(flat)
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commitOID := repo.putObject("commit", buildCommit(treeOID, []string{tip}, msg))
	repo.setRef("refs/heads/"+branch, commitOID)
	return jsonOK(`{"commit":{"sha":` + jstr(commitOID) + `}}`)
}

// ---- issues ----

// issuesEndpoint serves the issue routes.
func issuesEndpoint(req *request, store *Store, repo *Repo, sub []string) response {
	if len(sub) == 0 {
		switch req.method {
		case "POST":
			return createIssue(req, store, repo)
		case "GET":
			return listIssues(req, repo)
		}
		return jsonErr(404, "Not Found")
	}
	n, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 1 {
		switch req.method {
		case "GET":
			if is := repo.findIssue(n); is != nil {
				return jsonOK(is.issueJSON(repo))
			}
			return jsonErr(404, "Not Found")
		case "PATCH":
			return patchIssue(req, repo, n)
		}
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 2 && sub[1] == "comments" {
		switch req.method {
		case "GET":
			if is := repo.findIssue(n); is != nil {
				var b strings.Builder
				b.WriteByte('[')
				for i, c := range is.Comments {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(commentJSON(c))
				}
				b.WriteByte(']')
				return jsonOK(b.String())
			}
			return jsonErr(404, "Not Found")
		case "POST":
			return createComment(req, store, repo, n)
		}
	}
	return jsonErr(404, "Not Found")
}

func createIssue(req *request, store *Store, repo *Repo) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	var labels []string
	for _, item := range body.ArrField("labels") {
		switch item.Kind {
		case 's':
			labels = append(labels, item.Str)
		case 'o':
			labels = append(labels, item.StrField("name", ""))
		}
	}
	for _, name := range labels {
		if _, ok := repo.LabelMap[name]; !ok {
			return jsonErr(422, "Label '"+name+"' does not exist")
		}
	}
	is := &Issue{
		Number: repo.NextNum,
		ID:     store.NextObjID,
		Title:  body.StrField("title", ""),
		Body:   body.StrField("body", ""),
		State:  "open",
		Labels: labels,
	}
	repo.NextNum++
	store.NextObjID++
	repo.Issues = append(repo.Issues, is)
	repo.bump()
	return response{status: 201, ctype: "application/json; charset=utf-8", body: []byte(is.issueJSON(repo))}
}

func listIssues(req *request, repo *Repo) response {
	state := req.query("state")
	if state == "" {
		state = "open"
	}
	direction := req.query("direction")
	var filtered []*Issue
	for _, is := range repo.Issues {
		if state == "all" || is.State == state {
			filtered = append(filtered, is)
		}
	}
	if direction == "desc" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	perPage, page := parsePage(req)
	start, end, last := paginate(len(filtered), perPage, page)
	var b strings.Builder
	b.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte(',')
		}
		b.WriteString(filtered[i].issueJSON(repo))
	}
	b.WriteByte(']')
	resp := jsonOK(b.String())
	if link := linkHeader(req, perPage, page, last); link != "" {
		resp.extra = map[string]string{"Link": link}
	}
	return resp
}

func patchIssue(req *request, repo *Repo, n int64) response {
	is := repo.findIssue(n)
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	if f := body.Get("state"); f != nil && f.Kind == 's' && (f.Str == "open" || f.Str == "closed") {
		is.State = f.Str
	}
	if f := body.Get("title"); f != nil && f.Kind == 's' {
		is.Title = f.Str
	}
	if f := body.Get("body"); f != nil && f.Kind == 's' {
		is.Body = f.Str
	}
	is.cached = ""
	repo.bump()
	return jsonOK(is.issueJSON(repo))
}

func createComment(req *request, store *Store, repo *Repo, n int64) response {
	is := repo.findIssue(n)
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	c := &Comment{ID: store.NextObjID, Body: body.StrField("body", "")}
	store.NextObjID++
	is.Comments = append(is.Comments, c)
	is.cached = ""
	repo.bump()
	return response{status: 201, ctype: "application/json; charset=utf-8", body: []byte(commentJSON(c))}
}

// ---- pull requests ----

// pullsEndpoint serves the pull request routes.
func pullsEndpoint(req *request, store *Store, repo *Repo, sub []string) response {
	if len(sub) == 0 {
		switch req.method {
		case "POST":
			return createPR(req, store, repo)
		case "GET":
			return listPRs(req, repo)
		}
		return jsonErr(404, "Not Found")
	}
	n, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 1 {
		if req.method == "GET" {
			if pr := repo.findPR(n); pr != nil {
				return jsonOK(pr.prJSON(repo))
			}
		}
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 2 && sub[1] == "files" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return prFiles(req, repo, n)
	}
	if len(sub) == 2 && sub[1] == "merge" {
		if req.method != "PUT" {
			return jsonErr(404, "Not Found")
		}
		return mergePR(req, repo, n)
	}
	return jsonErr(404, "Not Found")
}

// createPRFromArgs creates a PR with the REST validation rules,
// returning the PR or an error message.
func createPRFromArgs(store *Store, repo *Repo, head, base, title, body string) (*PullRequest, string) {
	if _, ok := repo.Refs["refs/heads/"+head]; !ok {
		return nil, "head branch does not exist"
	}
	if _, ok := repo.Refs["refs/heads/"+base]; !ok {
		return nil, "base branch does not exist"
	}
	for _, pr := range repo.PRs {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, "A pull request already exists for " + head
		}
	}
	pr := &PullRequest{
		Number:  repo.NextNum,
		ID:      store.NextObjID,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
	}
	repo.NextNum++
	store.NextObjID++
	repo.PRs = append(repo.PRs, pr)
	repo.bump()
	return pr, ""
}

func createPR(req *request, store *Store, repo *Repo) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	pr, errMsg := createPRFromArgs(store, repo, body.StrField("head", ""), body.StrField("base", ""),
		body.StrField("title", ""), body.StrField("body", ""))
	if errMsg != "" {
		return jsonErr(422, errMsg)
	}
	return response{status: 201, ctype: "application/json; charset=utf-8", body: []byte(pr.prJSON(repo))}
}

func listPRs(req *request, repo *Repo) response {
	state := req.query("state")
	if state == "" {
		state = "open"
	}
	var filtered []*PullRequest
	for _, pr := range repo.PRs {
		if state == "all" || pr.State == state {
			filtered = append(filtered, pr)
		}
	}
	perPage, page := parsePage(req)
	start, end, last := paginate(len(filtered), perPage, page)
	var b strings.Builder
	b.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte(',')
		}
		b.WriteString(filtered[i].prJSON(repo))
	}
	b.WriteByte(']')
	resp := jsonOK(b.String())
	if link := linkHeader(req, perPage, page, last); link != "" {
		resp.extra = map[string]string{"Link": link}
	}
	return resp
}

// prDiff computes the merge-base diff for a PR, or nil when a tip
// is missing.
func prDiff(repo *Repo, pr *PullRequest) []FileDiff {
	headOID, ok := repo.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return nil
	}
	baseOID, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return nil
	}
	headCommit := repo.peelToCommit(headOID)
	baseCommit := repo.peelToCommit(baseOID)
	if headCommit == "" || baseCommit == "" {
		return nil
	}
	mb := repo.mergeBase(baseCommit, headCommit)
	baseTree := ""
	if mb != "" {
		baseTree = repo.treeOfCommit(mb)
	} else {
		baseTree = repo.treeOfCommit(baseCommit)
	}
	return repo.diffTrees(baseTree, repo.treeOfCommit(headCommit))
}

func prFiles(req *request, repo *Repo, n int64) response {
	pr := repo.findPR(n)
	if pr == nil {
		return jsonErr(404, "Not Found")
	}
	diffs := prDiff(repo, pr)
	if diffs == nil {
		return jsonErr(404, "Not Found")
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, d := range diffs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"filename":`)
		b.WriteString(jstr(d.Path))
		b.WriteString(`,"status":`)
		b.WriteString(jstr(d.Status))
		b.WriteString(`,"additions":`)
		b.WriteString(strconv.Itoa(d.Additions))
		b.WriteString(`,"deletions":`)
		b.WriteString(strconv.Itoa(d.Deletions))
		b.WriteString(`,"changes":`)
		b.WriteString(strconv.Itoa(d.Additions + d.Deletions))
		b.WriteString(`}`)
	}
	b.WriteByte(']')
	return jsonOK(b.String())
}

func mergePR(req *request, repo *Repo, n int64) response {
	pr := repo.findPR(n)
	if pr == nil {
		return jsonErr(404, "Not Found")
	}
	if pr.Merged {
		return jsonOK(`{"sha":` + jstr(pr.MergeSHA) + `,"merged":true,"message":"Pull request successfully merged"}`)
	}
	sha, st := repo.tryMerge(pr, false)
	switch st {
	case mergeOK:
		return jsonOK(`{"sha":` + jstr(sha) + `,"merged":true,"message":"Pull request successfully merged"}`)
	case mergeConflict:
		return jsonErr(405, "Merge conflict")
	default:
		return jsonErr(404, "Not Found")
	}
}
