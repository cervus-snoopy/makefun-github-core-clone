// MCP over POST /mcp: JSON-RPC 2.0, modern server/discover era with a legacy
// initialize fallback, and the six required tools.
package main

import (
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

const mcpToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"owner":{"type":"string","description":"Repository owner"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state: open, closed, or all"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func mcpHandler(req *httpRequest) *httpResponse {
	if req.method == "DELETE" {
		return jsonResp(200, []byte(`{}`))
	}
	if req.method != "POST" {
		return jsonErr(405, "Method Not Allowed")
	}
	body, ok := parseJSON(req.body)
	if !ok {
		return &httpResponse{status: 400, ctype: "application/json", body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	method := body.get("method").strOr("")
	id := body.get("id")
	idJSON := "null"
	if id != nil {
		idJSON = jvalToJSON(id)
	}
	if strings.HasPrefix(method, "notifications/") {
		return &httpResponse{status: 202, ctype: "application/json", noBody: true}
	}
	result := func(inner string) *httpResponse {
		return jsonResp(200, []byte(`{"jsonrpc":"2.0","id":`+idJSON+`,"result":`+inner+`}`))
	}
	errResp := func(code int, msg string) *httpResponse {
		var sb strings.Builder
		sb.WriteString(`{"jsonrpc":"2.0","id":`)
		sb.WriteString(idJSON)
		sb.WriteString(`,"error":{"code":`)
		sb.WriteString(strconv.Itoa(code))
		sb.WriteString(`,"message":`)
		jsonEscape(&sb, msg)
		sb.WriteString(`}}`)
		return jsonResp(200, []byte(sb.String()))
	}
	switch method {
	case "server/discover":
		return result(`{"supportedVersions":["2026-07-28","2025-11-25"],"capabilities":{"tools":{}},"serverInfo":{"name":` + jsonString(mcpServerName) + `,"version":` + jsonString(mcpServerVersion) + `},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":` + jsonString(mcpServerName) + `,"version":` + jsonString(mcpServerVersion) + `}}}`)
	case "initialize":
		params := body.get("params")
		ver := "2025-11-25"
		if params != nil {
			if v := params.get("protocolVersion"); v != nil && v.kind == 's' {
				switch v.str {
				case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
					ver = v.str
				}
			}
		}
		return result(`{"protocolVersion":` + jsonString(ver) + `,"capabilities":{"tools":{}},"serverInfo":{"name":` + jsonString(mcpServerName) + `,"version":` + jsonString(mcpServerVersion) + `}}`)
	case "tools/list":
		return result(mcpToolsJSON)
	case "tools/call":
		return mcpCall(body, idJSON)
	case "ping":
		return result(`{}`)
	}
	return errResp(-32601, "Method not found: "+method)
}

func mcpCall(body *jval, idJSON string) *httpResponse {
	errResp := func(code int, msg string) *httpResponse {
		var sb strings.Builder
		sb.WriteString(`{"jsonrpc":"2.0","id":`)
		sb.WriteString(idJSON)
		sb.WriteString(`,"error":{"code":`)
		sb.WriteString(strconv.Itoa(code))
		sb.WriteString(`,"message":`)
		jsonEscape(&sb, msg)
		sb.WriteString(`}}`)
		return jsonResp(200, []byte(sb.String()))
	}
	callResult := func(textJSON string, isError bool) *httpResponse {
		var sb strings.Builder
		sb.WriteString(`{"jsonrpc":"2.0","id":`)
		sb.WriteString(idJSON)
		sb.WriteString(`,"result":{"content":[{"type":"text","text":`)
		sb.WriteString(jsonString(textJSON))
		sb.WriteString(`}],"structuredContent":`)
		sb.WriteString(textJSON)
		if isError {
			sb.WriteString(`,"isError":true`)
		}
		sb.WriteString(`}}`)
		return jsonResp(200, []byte(sb.String()))
	}
	params := body.get("params")
	if params == nil {
		return errResp(-32602, "missing params")
	}
	name := params.get("name").strOr("")
	args := params.get("arguments")
	if args == nil {
		args = &jval{kind: 'o', obj: make(map[string]*jval)}
	}
	need := func(keys ...string) string {
		for _, k := range keys {
			v := args.get(k)
			if v == nil || (v.kind == 's' && v.str == "") {
				return k
			}
		}
		return ""
	}
	isErr := func(msg string) *httpResponse {
		return callResult(`{"error":`+jsonString(msg)+`}`, true)
	}
	switch name {
	case "create_repo":
		if missing := need("name"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		owner := args.get("owner").strOr(viewerLogin)
		if owner == "" {
			owner = viewerLogin
		}
		r, ok := store.createRepo(owner, args.get("name").strOr(""), args.get("description").strOr(""), args.get("private").boolOr(false))
		if !ok {
			return isErr("Repository already exists")
		}
		return callResult(string(store.repoJSON(r)), false)
	case "get_repo":
		if missing := need("owner", "repo"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		r := store.findRepo(args.get("owner").strOr(""), args.get("repo").strOr(""))
		if r == nil {
			return isErr("Repository not found")
		}
		return callResult(string(store.repoJSON(r)), false)
	case "create_issue":
		if missing := need("owner", "repo", "title"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		r := store.findRepo(args.get("owner").strOr(""), args.get("repo").strOr(""))
		if r == nil {
			return isErr("Repository not found")
		}
		is, errMsg := store.createIssue(r, args.get("title").strOr(""), args.get("body").strOr(""), nil)
		if errMsg != "" {
			return isErr(errMsg)
		}
		return callResult(string(store.issueJSON(r, is)), false)
	case "list_issues":
		if missing := need("owner", "repo"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		r := store.findRepo(args.get("owner").strOr(""), args.get("repo").strOr(""))
		if r == nil {
			return isErr("Repository not found")
		}
		state := args.get("state").strOr("open")
		var sb strings.Builder
		sb.WriteByte('[')
		first := true
		for _, is := range r.Issues {
			if state != "all" && is.State != state {
				continue
			}
			if !first {
				sb.WriteByte(',')
			}
			first = false
			sb.Write(store.issueJSON(r, is))
		}
		sb.WriteByte(']')
		return callResult(sb.String(), false)
	case "create_pull_request":
		if missing := need("owner", "repo", "title", "head", "base"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		r := store.findRepo(args.get("owner").strOr(""), args.get("repo").strOr(""))
		if r == nil {
			return isErr("Repository not found")
		}
		pr, errMsg := store.createPR(r, args.get("head").strOr(""), args.get("base").strOr(""), args.get("title").strOr(""), args.get("body").strOr(""))
		if errMsg != "" {
			return isErr(errMsg)
		}
		return callResult(string(store.prJSON(r, pr)), false)
	case "merge_pull_request":
		if missing := need("owner", "repo", "pull_number"); missing != "" {
			return isErr("missing required argument: " + missing)
		}
		r := store.findRepo(args.get("owner").strOr(""), args.get("repo").strOr(""))
		if r == nil {
			return isErr("Repository not found")
		}
		pr := r.findPR(args.get("pull_number").intOr(0))
		if pr == nil {
			return isErr("Pull request not found")
		}
		if !pr.Merged {
			sha, conflict, ok := store.mergePR(r, pr)
			if !ok {
				return isErr("Pull request not found")
			}
			if conflict {
				return isErr("Merge conflict")
			}
			_ = sha
		}
		var sb strings.Builder
		sb.WriteString(`{"number":`)
		sb.WriteString(strconv.Itoa(pr.Number))
		sb.WriteString(`,"merged":true,"sha":`)
		jsonEscape(&sb, pr.MergeSHA)
		sb.WriteString(`,"state":"closed"}`)
		return callResult(sb.String(), false)
	}
	return errResp(-32601, "Unknown tool: "+name)
}
