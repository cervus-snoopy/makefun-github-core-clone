package main

// MCP (Model Context Protocol): JSON-RPC 2.0 over HTTP at /mcp,
// with six tools.

import (
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

// toolsListJSON is the pre-built tools/list result, emitted
// byte-for-byte.
const toolsListJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"owner":{"type":"string","description":"Repository owner"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state: open, closed, or all"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

// rpcResult renders a JSON-RPC success envelope.
func rpcResult(id *V, result string) response {
	return response{status: 200, ctype: "application/json; charset=utf-8",
		body: []byte(`{"jsonrpc":"2.0","id":` + marshalV(id) + `,"result":` + result + `}`)}
}

// rpcError renders a JSON-RPC error envelope.
func rpcError(id *V, code int, msg string) response {
	return response{status: 200, ctype: "application/json; charset=utf-8",
		body: []byte(`{"jsonrpc":"2.0","id":` + marshalV(id) + `,"error":{"code":` + strconv.Itoa(code) + `,"message":` + jstr(msg) + `}}`)}
}

// toolResult renders a tools/call result envelope. The text field
// is the JSON encoding of the value; structuredContent is the
// value itself. isError appears only on tool-level errors.
func toolResult(id *V, valueJSON string, isError bool) response {
	var b strings.Builder
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.WriteString(marshalV(id))
	b.WriteString(`,"result":{"content":[{"type":"text","text":`)
	b.WriteString(jstr(valueJSON))
	b.WriteString(`}],"structuredContent":`)
	b.WriteString(valueJSON)
	if isError {
		b.WriteString(`,"isError":true`)
	}
	b.WriteString(`}}`)
	return response{status: 200, ctype: "application/json; charset=utf-8", body: []byte(b.String())}
}

// toolErr renders a tool-level error result.
func toolErr(id *V, msg string) response {
	return toolResult(id, `{"error":`+jstr(msg)+`}`, true)
}

// handleMCP serves every method on /mcp.
func handleMCP(req *request, store *Store) response {
	if req.method == "DELETE" {
		return jsonOK("{}")
	}
	if req.method != "POST" {
		return jsonErr(405, "Method Not Allowed")
	}
	body, ok := parseJSON(string(req.body))
	if !ok {
		return response{status: 400, ctype: "application/json; charset=utf-8",
			body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	id := body.Get("id")
	if id == nil {
		id = nullV()
	}
	method := body.StrField("method", "")
	if strings.HasPrefix(method, "notifications/") {
		return response{status: 202, ctype: "application/json; charset=utf-8", body: nil}
	}
	switch method {
	case "server/discover":
		return rpcResult(id, `{"supportedVersions":["2026-07-28","2025-11-25"],"capabilities":{"tools":{}},"serverInfo":{"name":"github-core-clone","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"github-core-clone","version":"1.0.0"}}}`)
	case "initialize":
		proto := "2025-11-25"
		if params := body.Get("params"); params != nil {
			if v := params.StrField("protocolVersion", ""); v != "" {
				switch v {
				case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
					proto = v
				}
			}
		}
		return rpcResult(id, `{"protocolVersion":`+jstr(proto)+`,"capabilities":{"tools":{}},"serverInfo":{"name":"github-core-clone","version":"1.0.0"}}`)
	case "ping":
		return rpcResult(id, `{}`)
	case "tools/list":
		return rpcResult(id, toolsListJSON)
	case "tools/call":
		return mcpCall(req, store, body, id)
	default:
		return rpcError(id, -32601, "Method not found: "+method)
	}
}

// mcpCall handles tools/call.
func mcpCall(req *request, store *Store, body *V, id *V) response {
	params := body.Get("params")
	if params == nil || params.Kind != 'o' {
		return rpcError(id, -32602, "missing params")
	}
	name := params.StrField("name", "")
	args := params.Get("arguments")
	if args == nil || args.Kind != 'o' {
		args = objV()
	}
	// Required-string validation, in the order of each tool's
	// required list. pull_number is validated separately.
	needStr := func(arg string) (string, bool) {
		s := args.StrField(arg, "")
		if s == "" {
			return "", false
		}
		return s, true
	}
	switch name {
	case "create_repo":
		nm, ok := needStr("name")
		if !ok {
			return toolErr(id, "missing required argument: name")
		}
		owner := args.StrField("owner", viewerLogin)
		if owner == "" {
			owner = viewerLogin
		}
		if store.getRepo(owner, nm) != nil {
			return toolErr(id, "Repository already exists")
		}
		repo := store.createRepo(owner, nm, args.StrField("description", ""), args.BoolField("private", false))
		return toolResult(id, repo.repoJSON(), false)
	case "get_repo":
		owner, ok1 := needStr("owner")
		if !ok1 {
			return toolErr(id, "missing required argument: owner")
		}
		rp, ok2 := needStr("repo")
		if !ok2 {
			return toolErr(id, "missing required argument: repo")
		}
		repo := store.getRepo(owner, rp)
		if repo == nil {
			return toolErr(id, "Repository not found")
		}
		return toolResult(id, repo.repoJSON(), false)
	case "create_issue":
		owner, ok1 := needStr("owner")
		if !ok1 {
			return toolErr(id, "missing required argument: owner")
		}
		rp, ok2 := needStr("repo")
		if !ok2 {
			return toolErr(id, "missing required argument: repo")
		}
		title, ok3 := needStr("title")
		if !ok3 {
			return toolErr(id, "missing required argument: title")
		}
		repo := store.getRepo(owner, rp)
		if repo == nil {
			return toolErr(id, "Repository not found")
		}
		is := &Issue{
			Number: repo.NextNum,
			ID:     store.NextObjID,
			Title:  title,
			Body:   args.StrField("body", ""),
			State:  "open",
		}
		repo.NextNum++
		store.NextObjID++
		repo.Issues = append(repo.Issues, is)
		repo.bump()
		return toolResult(id, is.issueJSON(repo), false)
	case "list_issues":
		owner, ok1 := needStr("owner")
		if !ok1 {
			return toolErr(id, "missing required argument: owner")
		}
		rp, ok2 := needStr("repo")
		if !ok2 {
			return toolErr(id, "missing required argument: repo")
		}
		repo := store.getRepo(owner, rp)
		if repo == nil {
			return toolErr(id, "Repository not found")
		}
		state := args.StrField("state", "open")
		var b strings.Builder
		b.WriteByte('[')
		first := true
		for _, is := range repo.Issues {
			if state != "all" && is.State != state {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(is.issueJSON(repo))
		}
		b.WriteByte(']')
		return toolResult(id, b.String(), false)
	case "create_pull_request":
		owner, ok1 := needStr("owner")
		if !ok1 {
			return toolErr(id, "missing required argument: owner")
		}
		rp, ok2 := needStr("repo")
		if !ok2 {
			return toolErr(id, "missing required argument: repo")
		}
		title, ok3 := needStr("title")
		if !ok3 {
			return toolErr(id, "missing required argument: title")
		}
		head, ok4 := needStr("head")
		if !ok4 {
			return toolErr(id, "missing required argument: head")
		}
		base, ok5 := needStr("base")
		if !ok5 {
			return toolErr(id, "missing required argument: base")
		}
		repo := store.getRepo(owner, rp)
		if repo == nil {
			return toolErr(id, "Repository not found")
		}
		pr, errMsg := createPRFromArgs(store, repo, head, base, title, args.StrField("body", ""))
		if errMsg != "" {
			return toolErr(id, errMsg)
		}
		return toolResult(id, pr.prJSON(repo), false)
	case "merge_pull_request":
		owner, ok1 := needStr("owner")
		if !ok1 {
			return toolErr(id, "missing required argument: owner")
		}
		rp, ok2 := needStr("repo")
		if !ok2 {
			return toolErr(id, "missing required argument: repo")
		}
		pn := args.Get("pull_number")
		if pn == nil {
			return toolErr(id, "missing required argument: pull_number")
		}
		repo := store.getRepo(owner, rp)
		if repo == nil {
			return toolErr(id, "Repository not found")
		}
		num := int64(0)
		if pn.Kind == 'n' {
			num = int64(pn.Num)
		}
		pr := repo.findPR(num)
		if pr == nil {
			return toolErr(id, "Pull request not found")
		}
		if pr.Merged {
			return toolResult(id, `{"number":`+strconv.FormatInt(pr.Number, 10)+`,"merged":true,"sha":`+jstr(pr.MergeSHA)+`,"state":"closed"}`, false)
		}
		sha, st := repo.tryMerge(pr, false)
		switch st {
		case mergeOK:
			return toolResult(id, `{"number":`+strconv.FormatInt(pr.Number, 10)+`,"merged":true,"sha":`+jstr(sha)+`,"state":"closed"}`, false)
		case mergeConflict:
			return toolErr(id, "Merge conflict")
		default:
			return toolErr(id, "Pull request not found")
		}
	default:
		return rpcError(id, -32601, "Unknown tool: "+name)
	}
}
