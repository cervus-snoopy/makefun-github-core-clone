package main

// REST API v3: dispatcher, pagination, repos, branches, git refs,
// commits and labels.

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// jsonErr builds a {"message":...} error response.
func jsonErr(status int, msg string) response {
	return response{status: status, ctype: "application/json; charset=utf-8", body: []byte(`{"message":` + jstr(msg) + `}`)}
}

// jsonOK builds a 200 JSON response from a pre-built body string.
func jsonOK(body string) response {
	return response{status: 200, ctype: "application/json; charset=utf-8", body: []byte(body)}
}

// parsePage reads per_page/page query parameters with the spec's
// defaults (30 and 1; unparseable/zero/negative fall back).
func parsePage(req *request) (int, int) {
	perPage := 30
	if s := req.query("per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if s := req.query("page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

// paginate slices a list and reports the total page count.
func paginate(total, perPage, page int) (int, int, int) {
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	last := (total + perPage - 1) / perPage
	if last < 1 {
		last = 1
	}
	return start, end, last
}

// linkHeader builds the pagination Link header when a next page
// exists, using the request host and the raw (undecoded) path.
func linkHeader(req *request, perPage, page, last int) string {
	if page >= last {
		return ""
	}
	base := "http://" + req.host + req.rawPath
	next := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
	lastURL := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(last)
	return "<" + next + `>; rel="next", <` + lastURL + `>; rel="last"`
}

// parseBodyJSON parses a request body as JSON. An empty body is
// an empty object; a parse failure is reported by the caller.
func parseBodyJSON(req *request) (*V, bool) {
	body := strings.TrimSpace(string(req.body))
	if body == "" {
		return objV(), true
	}
	return parseJSON(body)
}

// ownerGate checks the REST owner rule: only the viewer owner is
// addressable. It returns the repo or nil (caller sends 404).
func ownerGate(store *Store, owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return store.getRepo(owner, name)
}

// routeREST dispatches every /api/v3 path. It returns
// (handled, response); unhandled paths fall through to 404.
func routeREST(req *request, store *Store) (bool, response) {
	path := req.path
	if path == "/api/v3" || path == "/api/v3/" {
		if req.method != "GET" {
			return true, jsonErr(404, "Not Found")
		}
		return true, response{status: 200, ctype: "application/json; charset=utf-8", body: []byte("{}"),
			extra: map[string]string{"X-OAuth-Scopes": "repo, user"}}
	}
	if path == "/api/v3/meta" {
		if req.method != "GET" {
			return true, jsonErr(404, "Not Found")
		}
		return true, jsonOK(`{"installed_version":"1.0.0"}`)
	}
	if path == "/api/v3/user" {
		if req.method != "GET" {
			return true, jsonErr(404, "Not Found")
		}
		return true, jsonOK(`{"login":"adaiversen","id":1000,"name":"ada iversen","email":"adaiversen@example.invalid","type":"User"}`)
	}
	if path == "/api/v3/user/repos" {
		switch req.method {
		case "GET":
			return true, listUserRepos(req, store)
		case "POST":
			return true, createUserRepo(req, store)
		default:
			return true, jsonErr(404, "Not Found")
		}
	}
	if !strings.HasPrefix(path, "/api/v3/repos/") {
		return false, response{}
	}
	rest := strings.TrimPrefix(path, "/api/v3/repos/")
	segs := strings.Split(rest, "/")
	if len(segs) < 2 {
		return true, jsonErr(404, "Not Found")
	}
	owner, name := segs[0], segs[1]
	repo := ownerGate(store, owner, name)
	if repo == nil {
		return true, jsonErr(404, "Not Found")
	}
	sub := segs[2:]
	if len(sub) == 0 {
		return true, repoEndpoint(req, store, repo)
	}
	switch sub[0] {
	case "branches":
		return true, branchesEndpoint(req, repo, sub[1:])
	case "git":
		return true, gitRefsEndpoint(req, store, repo, sub[1:])
	case "commits":
		return true, commitsEndpoint(req, repo, sub[1:])
	case "contents":
		return true, contentsEndpoint(req, store, repo, sub[1:])
	case "labels":
		return true, labelsEndpoint(req, repo, sub[1:])
	case "issues":
		return true, issuesEndpoint(req, store, repo, sub[1:])
	case "pulls":
		return true, pullsEndpoint(req, store, repo, sub[1:])
	}
	return true, jsonErr(404, "Not Found")
}

// listUserRepos serves GET /api/v3/user/repos.
func listUserRepos(req *request, store *Store) response {
	var owned []*Repo
	for _, key := range store.RepoOrd {
		if r := store.Repos[key]; r != nil && r.Owner == viewerLogin {
			owned = append(owned, r)
		}
	}
	perPage, page := parsePage(req)
	start, end, last := paginate(len(owned), perPage, page)
	var b strings.Builder
	b.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte(',')
		}
		b.WriteString(owned[i].repoJSON())
	}
	b.WriteByte(']')
	resp := jsonOK(b.String())
	if link := linkHeader(req, perPage, page, last); link != "" {
		resp.extra = map[string]string{"Link": link}
	}
	return resp
}

// createUserRepo serves POST /api/v3/user/repos.
func createUserRepo(req *request, store *Store) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	name := body.StrField("name", "")
	if name == "" {
		return jsonErr(422, "name is required")
	}
	if store.getRepo(viewerLogin, name) != nil {
		return jsonErr(422, "Repository already exists")
	}
	repo := store.createRepo(viewerLogin, name, body.StrField("description", ""), body.BoolField("private", false))
	return response{status: 201, ctype: "application/json; charset=utf-8", body: []byte(repo.repoJSON())}
}

// repoEndpoint serves GET/PATCH/DELETE on a single repo.
func repoEndpoint(req *request, store *Store, repo *Repo) response {
	switch req.method {
	case "GET":
		return jsonOK(repo.repoJSON())
	case "PATCH":
		body, ok := parseBodyJSON(req)
		if !ok {
			return jsonErr(400, "Invalid JSON")
		}
		if f := body.Get("description"); f != nil && f.Kind == 's' {
			repo.Description = f.Str
		}
		if f := body.Get("private"); f != nil && f.Kind == 'b' {
			repo.Private = f.Bool
		}
		repo.bump()
		return jsonOK(repo.repoJSON())
	case "DELETE":
		store.deleteRepo(repo.Owner, repo.Name)
		return response{status: 204, ctype: "application/json; charset=utf-8", body: nil}
	}
	return jsonErr(404, "Not Found")
}

// branchJSON renders one branch object.
func branchJSON(name, sha string) string {
	return `{"name":` + jstr(name) + `,"commit":{"sha":` + jstr(sha) + `},"protected":false}`
}

// branchesEndpoint serves the branches routes.
func branchesEndpoint(req *request, repo *Repo, sub []string) response {
	if req.method != "GET" {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 0 {
		var b strings.Builder
		b.WriteByte('[')
		first := true
		for _, name := range repo.branchNames() {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(branchJSON(name, repo.Refs["refs/heads/"+name]))
		}
		b.WriteByte(']')
		return jsonOK(b.String())
	}
	if len(sub) == 1 {
		if oid, ok := repo.Refs["refs/heads/"+sub[0]]; ok {
			return jsonOK(branchJSON(sub[0], oid))
		}
	}
	return jsonErr(404, "Not Found")
}

// gitRefsEndpoint serves POST .../git/refs and GET .../git/refs/heads/{branch}.
func gitRefsEndpoint(req *request, store *Store, repo *Repo, sub []string) response {
	if req.method == "POST" && len(sub) == 1 && sub[0] == "refs" {
		body, ok := parseBodyJSON(req)
		if !ok {
			return jsonErr(400, "Invalid JSON")
		}
		ref := body.StrField("ref", "")
		sha := body.StrField("sha", "")
		if ref == "" || sha == "" {
			return jsonErr(422, "ref and sha are required")
		}
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			return jsonErr(422, "invalid ref")
		}
		if _, ok := repo.Refs[ref]; ok {
			return jsonErr(422, "Reference already exists")
		}
		if repo.getObject(sha) == nil {
			return jsonErr(422, "object does not exist")
		}
		repo.setRef(ref, sha)
		return response{status: 201, ctype: "application/json; charset=utf-8",
			body: []byte(`{"ref":` + jstr(ref) + `,"object":{"sha":` + jstr(sha) + `}}`)}
	}
	if req.method == "GET" && len(sub) == 3 && sub[0] == "refs" && sub[1] == "heads" {
		if oid, ok := repo.Refs["refs/heads/"+sub[2]]; ok {
			return jsonOK(`{"ref":` + jstr("refs/heads/"+sub[2]) + `,"object":{"sha":` + jstr(oid) + `}}`)
		}
	}
	return jsonErr(404, "Not Found")
}

// commitsEndpoint serves GET .../commits.
func commitsEndpoint(req *request, repo *Repo, sub []string) response {
	if req.method != "GET" || len(sub) != 0 {
		return jsonErr(404, "Not Found")
	}
	sha := req.query("sha")
	var tip string
	if sha == "" {
		tip = repo.Refs["refs/heads/main"]
		if tip == "" {
			return jsonOK("[]")
		}
	} else if oid, ok := repo.Refs["refs/heads/"+sha]; ok {
		tip = oid
	} else if repo.getObject(sha) != nil {
		tip = sha
	} else {
		return jsonErr(404, "Not Found")
	}
	commits := repo.collectCommits([]string{tip})
	var b strings.Builder
	b.WriteByte('[')
	for i, oid := range commits {
		if i > 0 {
			b.WriteByte(',')
		}
		o := repo.getObject(oid)
		ci := parseCommit(o.Data)
		au := parseWho(ci.Author)
		cm := parseWho(ci.Committer)
		msg := strings.TrimSuffix(ci.Message, "\n")
		b.WriteString(`{"sha":`)
		b.WriteString(jstr(oid))
		b.WriteString(`,"commit":{"message":`)
		b.WriteString(jstr(msg))
		b.WriteString(`,"author":{"name":`)
		b.WriteString(jstr(au.Name))
		b.WriteString(`,"email":`)
		b.WriteString(jstr(au.Email))
		b.WriteString(`,"date":`)
		b.WriteString(jstr(formatISO(au.TS)))
		b.WriteString(`},"committer":{"name":`)
		b.WriteString(jstr(cm.Name))
		b.WriteString(`,"email":`)
		b.WriteString(jstr(cm.Email))
		b.WriteString(`,"date":`)
		b.WriteString(jstr(formatISO(cm.TS)))
		b.WriteString(`}}}`)
	}
	b.WriteByte(']')
	return jsonOK(b.String())
}

// labelsEndpoint serves GET/POST .../labels.
func labelsEndpoint(req *request, repo *Repo, sub []string) response {
	if len(sub) != 0 {
		return jsonErr(404, "Not Found")
	}
	switch req.method {
	case "GET":
		var b strings.Builder
		b.WriteByte('[')
		for i, name := range repo.LabelOrd {
			if i > 0 {
				b.WriteByte(',')
			}
			lb := repo.LabelMap[name]
			b.WriteString(`{"name":` + jstr(lb.Name) + `,"color":` + jstr(lb.Color) + `}`)
		}
		b.WriteByte(']')
		return jsonOK(b.String())
	case "POST":
		body, ok := parseBodyJSON(req)
		if !ok {
			return jsonErr(400, "Invalid JSON")
		}
		name := body.StrField("name", "")
		if name == "" {
			return jsonErr(422, "name is required")
		}
		if lb, ok := repo.LabelMap[name]; ok {
			return response{status: 201, ctype: "application/json; charset=utf-8",
				body: []byte(`{"name":` + jstr(lb.Name) + `,"color":` + jstr(lb.Color) + `}`)}
		}
		color := body.StrField("color", "ededed")
		if color == "" {
			color = "ededed"
		}
		repo.LabelMap[name] = &Label{Name: name, Color: color}
		repo.LabelOrd = append(repo.LabelOrd, name)
		repo.bump()
		return response{status: 201, ctype: "application/json; charset=utf-8",
			body: []byte(`{"name":` + jstr(name) + `,"color":` + jstr(color) + `}`)}
	}
	return jsonErr(404, "Not Found")
}

// stripB64Whitespace removes \r \n \t and spaces from a base64
// string before decoding.
func stripB64Whitespace(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\r' && c != '\n' && c != '\t' && c != ' ' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// decodeB64 decodes standard base64 after whitespace stripping.
func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(stripB64Whitespace(s))
}
