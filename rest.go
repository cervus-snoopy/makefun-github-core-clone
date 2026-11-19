// REST v3 handlers and the top-level dispatcher. One guard mutex serializes
// request handling: the workloads are sequential or cache-served, so the
// serialization costs nothing and removes every store race.
package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

var reqGuard sync.Mutex

func dispatch(req *httpRequest) *httpResponse {
	reqGuard.Lock()
	defer reqGuard.Unlock()

	if req.path == "/healthz" && req.method == "GET" {
		return jsonResp(200, []byte(`{"status":"ok"}`))
	}
	if !authed(req) {
		return jsonErr(401, "Requires authentication")
	}
	if resp := gitRoute(req); resp != nil {
		return resp
	}
	if req.path == "/api/graphql" {
		if req.method != "POST" {
			return jsonErr(405, "Method Not Allowed")
		}
		return graphqlHandler(req)
	}
	if req.path == "/mcp" {
		return mcpHandler(req)
	}
	if strings.HasPrefix(req.path, "/api/v3") {
		return restRoute(req)
	}
	return jsonErr(404, "Not Found")
}

func authed(req *httpRequest) bool {
	if store.token == "" {
		return true
	}
	return req.headers["authorization"] == "Bearer "+store.token
}

func jsonResp(status int, body []byte) *httpResponse {
	return &httpResponse{status: status, ctype: "application/json; charset=utf-8", body: body}
}

func jsonErr(status int, msg string) *httpResponse {
	var sb strings.Builder
	sb.WriteString(`{"message":`)
	jsonEscape(&sb, msg)
	sb.WriteByte('}')
	return jsonResp(status, []byte(sb.String()))
}

func parseBody(req *httpRequest) (*jval, *httpResponse) {
	if len(req.body) == 0 {
		return &jval{kind: 'o', obj: make(map[string]*jval)}, nil
	}
	v, ok := parseJSON(req.body)
	if !ok {
		return nil, jsonErr(400, "Invalid JSON")
	}
	return v, nil
}

func pageParams(req *httpRequest) (perPage, page int) {
	perPage = 30
	if v := queryValue(req.query, "per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page = 1
	if v := queryValue(req.query, "page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

func linkHeader(req *httpRequest, perPage, page, total int) string {
	pages := (total + perPage - 1) / perPage
	if pages <= 1 || page >= pages {
		return ""
	}
	base := "http://" + req.host + req.rawPath
	next := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
	last := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(pages)
	return `<` + next + `>; rel="next", <` + last + `>; rel="last"`
}

func withLink(resp *httpResponse, link string) *httpResponse {
	if link != "" {
		resp.extra = append(resp.extra, "Link: "+link)
	}
	return resp
}

// ---------------------------------------------------------------- REST routing

func restRoute(req *httpRequest) *httpResponse {
	p := req.path
	if p == "/api/v3/" || p == "/api/v3" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		resp := jsonResp(200, []byte(`{}`))
		resp.extra = append(resp.extra, "X-OAuth-Scopes: repo, user")
		return resp
	}
	if p == "/api/v3/meta" && req.method == "GET" {
		return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
	}
	if p == "/api/v3/user" && req.method == "GET" {
		return jsonResp(200, []byte(`{"login":"adaiversen","id":1000,"name":"ada iversen","email":"adaiversen@example.invalid","type":"User"}`))
	}
	if p == "/api/v3/user/repos" {
		if req.method == "GET" {
			return listUserRepos(req)
		}
		if req.method == "POST" {
			return createUserRepo(req)
		}
		return jsonErr(404, "Not Found")
	}
	if strings.HasPrefix(p, "/api/v3/repos/") {
		return reposRoute(req)
	}
	return jsonErr(404, "Not Found")
}

func listUserRepos(req *httpRequest) *httpResponse {
	perPage, page := pageParams(req)
	var mine []*repo
	for _, r := range store.repoOrder {
		if r.Owner == viewerLogin {
			mine = append(mine, r)
		}
	}
	total := len(mine)
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
		sb.Write(store.repoJSON(mine[i]))
	}
	sb.WriteByte(']')
	return withLink(jsonResp(200, []byte(sb.String())), linkHeader(req, perPage, page, total))
}

func createUserRepo(req *httpRequest) *httpResponse {
	body, errResp := parseBody(req)
	if errResp != nil {
		return errResp
	}
	name := body.get("name").strOr("")
	if name == "" {
		return jsonErr(422, "name is required")
	}
	desc := body.get("description").strOr("")
	priv := body.get("private").boolOr(false)
	r, ok := store.createRepo(viewerLogin, name, desc, priv)
	if !ok {
		return jsonErr(422, "Repository already exists")
	}
	return jsonResp(201, store.repoJSON(r))
}

// ---------------------------------------------------------------- /repos/{o}/{r}...

func reposRoute(req *httpRequest) *httpResponse {
	rest := strings.TrimPrefix(req.path, "/api/v3/repos/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return jsonErr(404, "Not Found")
	}
	owner, name := parts[0], parts[1]
	if owner != viewerLogin {
		return jsonErr(404, "Not Found")
	}
	r := store.findRepo(owner, name)
	sub := parts[2:]
	if len(sub) == 0 {
		switch req.method {
		case "GET":
			if r == nil {
				return jsonErr(404, "Not Found")
			}
			return jsonResp(200, store.repoJSON(r))
		case "PATCH":
			if r == nil {
				return jsonErr(404, "Not Found")
			}
			body, errResp := parseBody(req)
			if errResp != nil {
				return errResp
			}
			if d := body.get("description"); d != nil && d.kind == 's' {
				r.Description = d.str
			}
			if pv := body.get("private"); pv != nil && pv.kind == 'b' {
				r.Private = pv.b
			}
			r.bump()
			return jsonResp(200, store.repoJSON(r))
		case "DELETE":
			if r == nil {
				return jsonErr(404, "Not Found")
			}
			store.deleteRepo(r)
			return &httpResponse{status: 204, ctype: "application/json; charset=utf-8", noBody: true}
		}
		return jsonErr(404, "Not Found")
	}
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	switch sub[0] {
	case "branches":
		return branchesRoute(req, r, sub[1:])
	case "git":
		return gitRefsRoute(req, r, sub[1:])
	case "commits":
		return commitsRoute(req, r)
	case "contents":
		return contentsRoute(req, r, sub[1:])
	case "labels":
		return labelsRoute(req, r)
	case "issues":
		return issuesRoute(req, r, sub[1:])
	case "pulls":
		return pullsRoute(req, r, sub[1:])
	}
	return jsonErr(404, "Not Found")
}

func branchesRoute(req *httpRequest, r *repo, sub []string) *httpResponse {
	if req.method == "GET" && len(sub) == 0 {
		var sb strings.Builder
		sb.WriteByte('[')
		first := true
		for _, name := range r.RefOrder {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			if !first {
				sb.WriteByte(',')
			}
			first = false
			branch := strings.TrimPrefix(name, "refs/heads/")
			sb.WriteString(`{"name":`)
			jsonEscape(&sb, branch)
			sb.WriteString(`,"commit":{"sha":`)
			jsonEscape(&sb, r.Refs[name])
			sb.WriteString(`},"protected":false}`)
		}
		sb.WriteByte(']')
		return jsonResp(200, []byte(sb.String()))
	}
	if req.method == "GET" && len(sub) == 1 {
		oid, ok := r.refTip("refs/heads/" + sub[0])
		if !ok {
			return jsonErr(404, "Not Found")
		}
		var sb strings.Builder
		sb.WriteString(`{"name":`)
		jsonEscape(&sb, sub[0])
		sb.WriteString(`,"commit":{"sha":`)
		jsonEscape(&sb, oid)
		sb.WriteString(`},"protected":false}`)
		return jsonResp(200, []byte(sb.String()))
	}
	return jsonErr(404, "Not Found")
}

func gitRefsRoute(req *httpRequest, r *repo, sub []string) *httpResponse {
	if req.method == "POST" && len(sub) == 1 && sub[0] == "refs" {
		body, errResp := parseBody(req)
		if errResp != nil {
			return errResp
		}
		ref := body.get("ref").strOr("")
		sha := body.get("sha").strOr("")
		if ref == "" || sha == "" {
			return jsonErr(422, "ref and sha are required")
		}
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			return jsonErr(422, "invalid ref")
		}
		if _, ok := r.Refs[ref]; ok {
			return jsonErr(422, "Reference already exists")
		}
		if r.getObject(sha) == nil {
			return jsonErr(422, "object does not exist")
		}
		r.setRef(ref, sha)
		var sb strings.Builder
		sb.WriteString(`{"ref":`)
		jsonEscape(&sb, ref)
		sb.WriteString(`,"object":{"sha":`)
		jsonEscape(&sb, sha)
		sb.WriteString(`}}`)
		return jsonResp(201, []byte(sb.String()))
	}
	if req.method == "GET" && len(sub) == 3 && sub[0] == "refs" && sub[1] == "heads" {
		oid, ok := r.refTip("refs/heads/" + sub[2])
		if !ok {
			return jsonErr(404, "Not Found")
		}
		var sb strings.Builder
		sb.WriteString(`{"ref":`)
		jsonEscape(&sb, "refs/heads/"+sub[2])
		sb.WriteString(`,"object":{"sha":`)
		jsonEscape(&sb, oid)
		sb.WriteString(`}}`)
		return jsonResp(200, []byte(sb.String()))
	}
	return jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------- commits

func parseWho(line string) (name, email string, ts int64) {
	// "name <email> 1735689600 +0000"
	lt := strings.IndexByte(line, '<')
	gt := strings.IndexByte(line, '>')
	if lt < 0 || gt < 0 {
		return line, "", fixedEpoch
	}
	name = strings.TrimSpace(line[:lt])
	email = line[lt+1 : gt]
	rest := strings.TrimSpace(line[gt+1:])
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			ts = n
		}
	}
	return name, email, ts
}

func tsISO(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func commitsRoute(req *httpRequest, r *repo) *httpResponse {
	if req.method != "GET" {
		return jsonErr(404, "Not Found")
	}
	shaParam := queryValue(req.query, "sha")
	var tip string
	if shaParam == "" {
		var ok bool
		tip, ok = r.defaultTip()
		if !ok {
			return jsonResp(200, []byte(`[]`))
		}
	} else if oid, ok := r.refTip("refs/heads/" + shaParam); ok {
		tip = oid
	} else if r.getObject(shaParam) != nil {
		tip = shaParam
	} else {
		return jsonErr(404, "Not Found")
	}
	commits := collectCommits(r.getObject, []string{tip})
	var sb strings.Builder
	sb.WriteByte('[')
	for i, oid := range commits {
		if i > 0 {
			sb.WriteByte(',')
		}
		c := parseCommit(r.getObject(oid).data)
		msg := strings.TrimSuffix(c.message, "\n")
		an, ae, ats := parseWho(c.author)
		cn, ce, cts := parseWho(c.committer)
		sb.WriteString(`{"sha":`)
		jsonEscape(&sb, oid)
		sb.WriteString(`,"commit":{"message":`)
		jsonEscape(&sb, msg)
		sb.WriteString(`,"author":{"name":`)
		jsonEscape(&sb, an)
		sb.WriteString(`,"email":`)
		jsonEscape(&sb, ae)
		sb.WriteString(`,"date":`)
		jsonEscape(&sb, tsISO(ats))
		sb.WriteString(`},"committer":{"name":`)
		jsonEscape(&sb, cn)
		sb.WriteString(`,"email":`)
		jsonEscape(&sb, ce)
		sb.WriteString(`,"date":`)
		jsonEscape(&sb, tsISO(cts))
		sb.WriteString(`}}}`)
	}
	sb.WriteByte(']')
	return jsonResp(200, []byte(sb.String()))
}

// ---------------------------------------------------------------- labels

func labelsRoute(req *httpRequest, r *repo) *httpResponse {
	if req.method == "GET" {
		var sb strings.Builder
		sb.WriteByte('[')
		for i, name := range r.LabelOrder {
			if i > 0 {
				sb.WriteByte(',')
			}
			lb := r.Labels[name]
			sb.WriteString(`{"name":`)
			jsonEscape(&sb, lb.Name)
			sb.WriteString(`,"color":`)
			jsonEscape(&sb, lb.Color)
			sb.WriteByte('}')
		}
		sb.WriteByte(']')
		return jsonResp(200, []byte(sb.String()))
	}
	if req.method == "POST" {
		body, errResp := parseBody(req)
		if errResp != nil {
			return errResp
		}
		name := body.get("name").strOr("")
		if name == "" {
			return jsonErr(422, "name is required")
		}
		color := body.get("color").strOr("ededed")
		if _, ok := r.Labels[name]; !ok {
			r.Labels[name] = &label{Name: name, Color: color}
			r.LabelOrder = append(r.LabelOrder, name)
			r.bump()
		}
		lb := r.Labels[name]
		var sb strings.Builder
		sb.WriteString(`{"name":`)
		jsonEscape(&sb, lb.Name)
		sb.WriteString(`,"color":`)
		jsonEscape(&sb, lb.Color)
		sb.WriteByte('}')
		return jsonResp(201, []byte(sb.String()))
	}
	return jsonErr(404, "Not Found")
}
