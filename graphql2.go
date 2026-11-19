// GraphQL executor: field resolution, result serialization, HTTP handler.
package main

import (
	"strconv"
	"strings"
)

func graphqlHandler(req *httpRequest) *httpResponse {
	body, ok := parseJSON(req.body)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	if body.kind == 'a' {
		var parts []string
		for _, item := range body.arr {
			parts = append(parts, executeGraphQL(item))
		}
		return jsonResp(200, []byte("["+strings.Join(parts, ",")+"]"))
	}
	return jsonResp(200, []byte(executeGraphQL(body)))
}

func executeGraphQL(body *jval) string {
	query := body.get("query").strOr("")
	vars := map[string]*jval{}
	if v := body.get("variables"); v != nil && v.kind == 'o' {
		vars = v.obj
	}
	doc, ok := parseGraphQLDocument(query)
	var errs []string
	var data string
	if !ok {
		errs = append(errs, "Syntax Error: could not parse query")
		data = "null"
	} else {
		rootKind := "query"
		if doc.kind == "mutation" {
			rootKind = "mutation"
		}
		root := &gnode{kind: rootKind}
		data = "{" + execBody(root, doc.fields, vars, &errs) + "}"
	}
	var sb strings.Builder
	sb.WriteString(`{"data":`)
	sb.WriteString(data)
	if len(errs) > 0 {
		sb.WriteString(`,"errors":[`)
		for i, e := range errs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"message":`)
			jsonEscape(&sb, e)
			sb.WriteByte('}')
		}
		sb.WriteByte(']')
	}
	sb.WriteByte('}')
	return sb.String()
}

func execBody(parent *gnode, fields []*gqlField, vars map[string]*jval, errs *[]string) string {
	var parts []string
	for _, f := range fields {
		if f.fragOn != "" {
			if parent.typeName() != f.fragOn {
				continue
			}
			if inner := execBody(parent, f.sel, vars, errs); inner != "" {
				parts = append(parts, inner)
			}
			continue
		}
		parts = append(parts, jsonString(f.alias)+":"+execValue(parent, f, vars, errs))
	}
	return strings.Join(parts, ",")
}

func execValue(parent *gnode, f *gqlField, vars map[string]*jval, errs *[]string) string {
	if f.name == "__typename" {
		return jsonString(parent.typeName())
	}
	child := resolve(parent, f, vars, errs)
	if child == nil {
		return "null"
	}
	switch child.kind {
	case "scalar":
		return jvalToJSON(child.scalar)
	case "list":
		var items []string
		for _, item := range child.list {
			items = append(items, "{"+execBody(item, f.sel, vars, errs)+"}")
		}
		return "[" + strings.Join(items, ",") + "]"
	default:
		return "{" + execBody(child, f.sel, vars, errs) + "}"
	}
}

func jvalToJSON(v *jval) string {
	if v == nil {
		return "null"
	}
	switch v.kind {
	case 's':
		return jsonString(v.str)
	case 'n':
		if v.num == float64(int64(v.num)) {
			return strconv.FormatInt(int64(v.num), 10)
		}
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	case 'b':
		if v.b {
			return "true"
		}
		return "false"
	default:
		return "null"
	}
}

// ---------------------------------------------------------------- resolution

func (f *gqlField) arg(name string, vars map[string]*jval) *jval {
	if a, ok := f.args[name]; ok {
		return a.toJval(vars)
	}
	return nil
}

func decodeRepoID(id string) *repo {
	kind, key, ok := parseGlobalID(id)
	if !ok || kind != "Repository" {
		return nil
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return store.findRepo(parts[0], parts[1])
}

func decodeIssueID(id string) (*repo, int, bool) {
	kind, key, ok := parseGlobalID(id)
	if !ok || kind != "Issue" {
		return nil, 0, false
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return nil, 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, 0, false
	}
	return store.findRepo(parts[0], parts[1]), n, true
}

func decodePRID(id string) (*repo, int, bool) {
	kind, key, ok := parseGlobalID(id)
	if !ok || kind != "PullRequest" {
		return nil, 0, false
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return nil, 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, 0, false
	}
	return store.findRepo(parts[0], parts[1]), n, true
}

func issueNode(r *repo, is *issue) *gnode    { return &gnode{kind: "issue", r: r, issue: is} }
func prNode(r *repo, pr *pullRequest) *gnode { return &gnode{kind: "pr", r: r, pr: pr} }

func resolve(parent *gnode, f *gqlField, vars map[string]*jval, errs *[]string) *gnode {
	switch parent.kind {
	case "query":
		switch f.name {
		case "viewer":
			return &gnode{kind: "user"}
		case "repository":
			owner := f.arg("owner", vars).strOr("")
			name := f.arg("name", vars).strOr("")
			if owner != viewerLogin {
				return nil
			}
			r := store.findRepo(owner, name)
			if r == nil {
				return nil
			}
			return &gnode{kind: "repo", r: r}
		case "node":
			id := f.arg("id", vars).strOr("")
			kind, key, ok := parseGlobalID(id)
			if !ok {
				return nil
			}
			parts := strings.Split(key, "/")
			switch kind {
			case "Repository":
				if len(parts) == 2 {
					if r := store.findRepo(parts[0], parts[1]); r != nil {
						return &gnode{kind: "repo", r: r}
					}
				}
			case "Issue":
				if len(parts) == 3 {
					if r := store.findRepo(parts[0], parts[1]); r != nil {
						if n, err := strconv.Atoi(parts[2]); err == nil {
							if is := r.findIssue(n); is != nil {
								return issueNode(r, is)
							}
						}
					}
				}
			case "PullRequest":
				if len(parts) == 3 {
					if r := store.findRepo(parts[0], parts[1]); r != nil {
						if n, err := strconv.Atoi(parts[2]); err == nil {
							if pr := r.findPR(n); pr != nil {
								return prNode(r, pr)
							}
						}
					}
				}
			}
			return nil
		case "__type":
			name := f.arg("name", vars).strOr("")
			switch name {
			case "Issue", "PullRequest", "Repository", "SearchType", "User", "Query", "Mutation":
				return &gnode{kind: "type", tn: name}
			}
			return nil
		}
	case "mutation":
		switch f.name {
		case "createIssue":
			input := f.arg("input", vars)
			r := decodeRepoID(input.get("repositoryId").strOr(""))
			if r == nil {
				*errs = append(*errs, "Repository not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"issue": nil}}
			}
			is, errMsg := store.createIssue(r, input.get("title").strOr(""), input.get("body").strOr(""), nil)
			if errMsg != "" {
				*errs = append(*errs, errMsg)
				return &gnode{kind: "payload", payload: map[string]*gnode{"issue": nil}}
			}
			return &gnode{kind: "payload", payload: map[string]*gnode{"issue": issueNode(r, is)}}
		case "closeIssue":
			input := f.arg("input", vars)
			r, n, ok := decodeIssueID(input.get("issueId").strOr(""))
			if !ok || r == nil {
				*errs = append(*errs, "Issue not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"issue": nil}}
			}
			is := r.findIssue(n)
			if is == nil {
				*errs = append(*errs, "Issue not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"issue": nil}}
			}
			is.State = "closed"
			is.cached = nil
			return &gnode{kind: "payload", payload: map[string]*gnode{"issue": issueNode(r, is)}}
		case "createPullRequest":
			input := f.arg("input", vars)
			r := decodeRepoID(input.get("repositoryId").strOr(""))
			if r == nil {
				*errs = append(*errs, "Repository not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": nil}}
			}
			pr, errMsg := store.createPR(r, input.get("headRefName").strOr(""), input.get("baseRefName").strOr(""), input.get("title").strOr(""), input.get("body").strOr(""))
			if errMsg != "" {
				*errs = append(*errs, errMsg)
				return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": nil}}
			}
			return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": prNode(r, pr)}}
		case "mergePullRequest":
			input := f.arg("input", vars)
			r, n, ok := decodePRID(input.get("pullRequestId").strOr(""))
			if !ok || r == nil {
				*errs = append(*errs, "Pull request not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": nil}}
			}
			pr := r.findPR(n)
			if pr == nil {
				*errs = append(*errs, "Pull request not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": nil}}
			}
			if !pr.Merged {
				store.mergePR(r, pr)
			}
			return &gnode{kind: "payload", payload: map[string]*gnode{"pullRequest": prNode(r, pr)}}
		case "addComment":
			input := f.arg("input", vars)
			r, n, ok := decodeIssueID(input.get("subjectId").strOr(""))
			if !ok || r == nil {
				*errs = append(*errs, "Subject not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"commentEdge": nil}}
			}
			is := r.findIssue(n)
			if is == nil {
				*errs = append(*errs, "Subject not found")
				return &gnode{kind: "payload", payload: map[string]*gnode{"commentEdge": nil}}
			}
			c := &comment{ID: store.nextID(), Body: input.get("body").strOr("")}
			is.Comments = append(is.Comments, c)
			is.cached = nil
			edge := &gnode{kind: "commentEdge", payload: map[string]*gnode{
				"node": {kind: "comment", r: r, issue: is, comment: c},
			}}
			return &gnode{kind: "payload", payload: map[string]*gnode{"commentEdge": edge}}
		}
	case "user":
		switch f.name {
		case "login":
			return scalarStr(viewerLogin)
		case "name":
			return scalarStr(viewerName)
		case "id":
			return scalarNum(viewerID)
		}
	case "repo":
		r := parent.r
		switch f.name {
		case "id":
			return scalarStr(globalID("Repository", r.Owner+"/"+r.Name))
		case "databaseId":
			return scalarNum(float64(r.ID))
		case "name":
			return scalarStr(r.Name)
		case "nameWithOwner":
			return scalarStr(r.Owner + "/" + r.Name)
		case "owner":
			return &gnode{kind: "user"}
		case "defaultBranchRef":
			return &gnode{kind: "branchRef"}
		case "viewerPermission":
			return scalarStr("ADMIN")
		case "hasIssuesEnabled":
			return scalarBool(true)
		case "issue":
			n := f.arg("number", vars).intOr(0)
			if is := r.findIssue(n); is != nil {
				return issueNode(r, is)
			}
			return nil
		case "issues":
			return issueConnection(r, f, vars)
		case "pullRequests":
			return prConnection(r, f, vars)
		case "pullRequest":
			n := f.arg("number", vars).intOr(0)
			if pr := r.findPR(n); pr != nil {
				return prNode(r, pr)
			}
			return nil
		}
	case "issue":
		is := parent.issue
		switch f.name {
		case "id":
			return scalarStr(globalID("Issue", parent.r.Owner+"/"+parent.r.Name+"/"+strconv.Itoa(is.Number)))
		case "number":
			return scalarNum(float64(is.Number))
		case "title":
			return scalarStr(is.Title)
		case "state":
			if is.State == "closed" {
				return scalarStr("CLOSED")
			}
			return scalarStr("OPEN")
		case "body":
			return scalarStr(is.Body)
		}
	case "pr":
		pr := parent.pr
		switch f.name {
		case "id":
			return scalarStr(globalID("PullRequest", parent.r.Owner+"/"+parent.r.Name+"/"+strconv.Itoa(pr.Number)))
		case "number":
			return scalarNum(float64(pr.Number))
		case "title":
			return scalarStr(pr.Title)
		case "state":
			if pr.Merged {
				return scalarStr("MERGED")
			}
			if pr.State == "closed" {
				return scalarStr("CLOSED")
			}
			return scalarStr("OPEN")
		case "merged":
			return scalarBool(pr.Merged)
		case "headRefName":
			return scalarStr(pr.HeadRef)
		case "baseRefName":
			return scalarStr(pr.BaseRef)
		case "mergeable":
			if pr.State != "open" {
				return scalarStr("UNKNOWN")
			}
			if store.prMergeable(parent.r, pr) {
				return scalarStr("MERGEABLE")
			}
			return scalarStr("CONFLICTING")
		case "files":
			return fileConnection(parent.r, pr, f, vars)
		}
	case "branchRef":
		if f.name == "name" {
			return scalarStr("main")
		}
	case "issueConn":
		switch f.name {
		case "nodes":
			list := &gnode{kind: "list"}
			for _, is := range parent.issues {
				list.list = append(list.list, issueNode(parent.r, is))
			}
			return list
		case "pageInfo":
			return &gnode{kind: "pageInfo", hasNext: parent.hasNext, endCur: parent.endCur}
		case "totalCount":
			return scalarNum(float64(len(parent.issues)))
		}
	case "prConn":
		switch f.name {
		case "nodes":
			list := &gnode{kind: "list"}
			for _, pr := range parent.prs {
				list.list = append(list.list, prNode(parent.r, pr))
			}
			return list
		case "pageInfo":
			return &gnode{kind: "pageInfo", hasNext: parent.hasNext, endCur: parent.endCur}
		case "totalCount":
			return scalarNum(float64(len(parent.prs)))
		}
	case "fileConn":
		switch f.name {
		case "nodes":
			list := &gnode{kind: "list"}
			for i := range parent.files {
				list.list = append(list.list, &gnode{kind: "file", file: &parent.files[i]})
			}
			return list
		case "pageInfo":
			return &gnode{kind: "pageInfo", hasNext: false, endCur: ""}
		}
	case "file":
		switch f.name {
		case "path":
			return scalarStr(parent.file.path)
		case "filename":
			return scalarStr(parent.file.path)
		}
	case "pageInfo":
		switch f.name {
		case "hasNextPage":
			return scalarBool(parent.hasNext)
		case "endCursor":
			if parent.endCur == "" {
				return nil
			}
			return scalarStr(parent.endCur)
		case "hasPreviousPage":
			return scalarBool(false)
		case "startCursor":
			return scalarStr("")
		}
	case "type":
		switch f.name {
		case "name":
			return scalarStr(parent.tn)
		case "kind":
			if parent.tn == "SearchType" {
				return scalarStr("ENUM")
			}
			return scalarStr("OBJECT")
		case "fields":
			list := &gnode{kind: "list"}
			for _, name := range typeFields(parent.tn) {
				list.list = append(list.list, &gnode{kind: "field", tn: name})
			}
			return list
		case "enumValues":
			list := &gnode{kind: "list"}
			if parent.tn == "SearchType" {
				for _, name := range []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT", "PULL_REQUEST"} {
					list.list = append(list.list, &gnode{kind: "enumVal", tn: name})
				}
			}
			return list
		}
	case "field":
		if f.name == "name" {
			return scalarStr(parent.tn)
		}
	case "enumVal":
		if f.name == "name" {
			return scalarStr(parent.tn)
		}
	case "comment":
		switch f.name {
		case "id":
			return scalarStr(globalID("IssueComment", parent.r.Owner+"/"+parent.r.Name+"/"+strconv.Itoa(parent.issue.Number)+"/"+strconv.Itoa(parent.comment.ID)))
		case "body":
			return scalarStr(parent.comment.Body)
		}
	case "commentEdge":
		if f.name == "node" {
			return parent.payload["node"]
		}
	case "payload":
		if n, ok := parent.payload[f.name]; ok {
			return n
		}
		return nil
	}
	*errs = append(*errs, `Cannot query field "`+f.name+`" on type "`+parent.typeName()+`".`)
	return nil
}

func typeFields(typeName string) []string {
	switch typeName {
	case "Issue":
		return []string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "closedAt", "author", "comments", "labels", "repository", "url"}
	case "PullRequest":
		return []string{"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt", "headRef", "baseRef"}
	case "Repository":
		return []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "updatedAt"}
	}
	return nil
}

func issueConnection(r *repo, f *gqlField, vars map[string]*jval) *gnode {
	states := map[string]bool{}
	if sv := f.arg("states", vars); sv != nil && sv.kind == 'a' {
		for _, item := range sv.arr {
			if item.kind == 's' {
				states[item.str] = true
			}
		}
	}
	var list []*issue
	for _, is := range r.Issues {
		if len(states) > 0 {
			st := "OPEN"
			if is.State == "closed" {
				st = "CLOSED"
			}
			if !states[st] {
				continue
			}
		}
		list = append(list, is)
	}
	desc := false
	if ob := f.arg("orderBy", vars); ob != nil && ob.kind == 'o' {
		if d := ob.get("direction"); d != nil && d.kind == 's' && d.str == "DESC" {
			desc = true
		}
	}
	if desc {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	if av := f.arg("after", vars); av != nil && av.kind == 's' && av.str != "" {
		if n, ok := cursorNum(av.str); ok {
			var filtered []*issue
			for _, is := range list {
				if desc {
					if is.Number < n {
						filtered = append(filtered, is)
					}
				} else if is.Number > n {
					filtered = append(filtered, is)
				}
			}
			list = filtered
		}
	}
	first := len(list)
	if fv := f.arg("first", vars); fv != nil && fv.kind == 'n' {
		if int(fv.num) < first {
			first = int(fv.num)
		}
	}
	hasNext := len(list) > first
	page := list[:first]
	endCur := ""
	if len(page) > 0 {
		endCur = cursorFor(page[len(page)-1].Number)
	}
	return &gnode{kind: "issueConn", r: r, issues: page, hasNext: hasNext, endCur: endCur}
}

func prConnection(r *repo, f *gqlField, vars map[string]*jval) *gnode {
	first := len(r.PRs)
	if fv := f.arg("first", vars); fv != nil && fv.kind == 'n' {
		if int(fv.num) < first {
			first = int(fv.num)
		}
	}
	hasNext := len(r.PRs) > first
	return &gnode{kind: "prConn", r: r, prs: r.PRs[:first], hasNext: hasNext}
}

func fileConnection(r *repo, pr *pullRequest, f *gqlField, vars map[string]*jval) *gnode {
	headSHA, ok1 := r.refTip("refs/heads/" + pr.HeadRef)
	baseSHA, ok2 := r.refTip("refs/heads/" + pr.BaseRef)
	var diffs []fileDiff
	if ok1 && ok2 {
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
		diffs = diffTrees(r.getObject, baseTree, headTree)
	}
	first := len(diffs)
	if fv := f.arg("first", vars); fv != nil && fv.kind == 'n' {
		if int(fv.num) < first {
			first = int(fv.num)
		}
	}
	return &gnode{kind: "fileConn", r: r, files: diffs[:first]}
}
