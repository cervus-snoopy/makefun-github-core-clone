package main

// GraphQL: a hand-written parser and executor for the
// Repository / Issue / PullRequest surface, with connections,
// cursor pagination, introspection and mutations.

import (
	"strconv"
	"strings"
)

// ---- parser ----

type gqlField struct {
	alias    string
	name     string
	args     map[string]*V
	children []*gqlField
	fragType string // non-empty for an inline fragment
}

type gqlOp struct {
	kind   string // "query" | "mutation"
	fields []*gqlField
}

type gqlParser struct {
	s   string
	pos int
	err bool
}

func (p *gqlParser) skip() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
		} else if c == '#' {
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		} else {
			return
		}
	}
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func (p *gqlParser) readName() string {
	p.skip()
	start := p.pos
	if p.pos < len(p.s) && isNameStart(p.s[p.pos]) {
		p.pos++
		for p.pos < len(p.s) && isNameChar(p.s[p.pos]) {
			p.pos++
		}
	}
	return p.s[start:p.pos]
}

func (p *gqlParser) peekName() string {
	save := p.pos
	n := p.readName()
	p.pos = save
	return n
}

// skipBalanced skips a balanced bracket group starting at the
// current '(' (or '[' / '{'), honouring strings.
func (p *gqlParser) skipBalanced() {
	p.skip()
	if p.pos >= len(p.s) {
		return
	}
	open := p.s[p.pos]
	var close byte
	switch open {
	case '(':
		close = ')'
	case '[':
		close = ']'
	case '{':
		close = '}'
	default:
		return
	}
	depth := 0
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '"' {
			p.pos++
			for p.pos < len(p.s) && p.s[p.pos] != '"' {
				if p.s[p.pos] == '\\' {
					p.pos++
				}
				p.pos++
			}
			if p.pos < len(p.s) {
				p.pos++
			}
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			p.pos++
			if depth == 0 {
				return
			}
			continue
		}
		p.pos++
	}
}

func parseGQLDoc(s string) (*gqlOp, bool) {
	p := &gqlParser{s: s}
	p.skip()
	op := &gqlOp{kind: "query"}
	if p.pos < len(p.s) && p.s[p.pos] == '{' {
		fields, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		op.fields = fields
		return op, true
	}
	kw := p.readName()
	switch kw {
	case "query", "mutation", "subscription":
		op.kind = kw
	default:
		return nil, false
	}
	// Optional operation name.
	if n := p.peekName(); n != "" {
		p.readName()
	}
	// Optional variable definitions: skip balanced parens.
	p.skip()
	if p.pos < len(p.s) && p.s[p.pos] == '(' {
		p.skipBalanced()
	}
	fields, ok := p.selectionSet()
	if !ok {
		return nil, false
	}
	op.fields = fields
	return op, true
}

func (p *gqlParser) selectionSet() ([]*gqlField, bool) {
	p.skip()
	if p.pos >= len(p.s) || p.s[p.pos] != '{' {
		p.err = true
		return nil, false
	}
	p.pos++
	var fields []*gqlField
	for {
		p.skip()
		if p.pos >= len(p.s) {
			return nil, false
		}
		if p.s[p.pos] == '}' {
			p.pos++
			return fields, true
		}
		f, ok := p.field()
		if !ok {
			return nil, false
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) field() (*gqlField, bool) {
	p.skip()
	// Inline fragment or fragment spread.
	if strings.HasPrefix(p.s[p.pos:], "...") {
		p.pos += 3
		p.skip()
		if strings.HasPrefix(p.s[p.pos:], "on") && !isNameChar(p.s[p.pos+2]) {
			p.pos += 2
			typ := p.readName()
			if typ == "" {
				return nil, false
			}
			children, ok := p.selectionSet()
			if !ok {
				return nil, false
			}
			return &gqlField{fragType: typ, children: children}, true
		}
		// Named fragment spread / definition: rejected.
		return nil, false
	}
	name := p.readName()
	if name == "" {
		return nil, false
	}
	f := &gqlField{name: name, alias: name}
	p.skip()
	if p.pos < len(p.s) && p.s[p.pos] == ':' {
		p.pos++
		real := p.readName()
		if real == "" {
			return nil, false
		}
		f.name = real
	}
	// Arguments.
	p.skip()
	if p.pos < len(p.s) && p.s[p.pos] == '(' {
		p.pos++
		f.args = map[string]*V{}
		for {
			p.skip()
			if p.pos >= len(p.s) {
				return nil, false
			}
			if p.s[p.pos] == ')' {
				p.pos++
				break
			}
			argName := p.readName()
			if argName == "" {
				return nil, false
			}
			p.skip()
			if p.pos >= len(p.s) || p.s[p.pos] != ':' {
				return nil, false
			}
			p.pos++
			val, ok := p.value()
			if !ok {
				return nil, false
			}
			f.args[argName] = val
		}
	}
	// Directives: consumed and ignored.
	for {
		p.skip()
		if p.pos < len(p.s) && p.s[p.pos] == '@' {
			p.pos++
			if p.readName() == "" {
				return nil, false
			}
			p.skip()
			if p.pos < len(p.s) && p.s[p.pos] == '(' {
				p.skipBalanced()
			}
			continue
		}
		break
	}
	// Nested selection set.
	p.skip()
	if p.pos < len(p.s) && p.s[p.pos] == '{' {
		children, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		f.children = children
	}
	return f, true
}

func (p *gqlParser) value() (*V, bool) {
	p.skip()
	if p.pos >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.pos]; {
	case c == '"':
		s, ok := p.gqlString()
		if !ok {
			return nil, false
		}
		return strV(s), true
	case c == '[':
		p.pos++
		v := arrV()
		for {
			p.skip()
			if p.pos >= len(p.s) {
				return nil, false
			}
			if p.s[p.pos] == ']' {
				p.pos++
				return v, true
			}
			item, ok := p.value()
			if !ok {
				return nil, false
			}
			v.Arr = append(v.Arr, item)
		}
	case c == '{':
		p.pos++
		v := objV()
		for {
			p.skip()
			if p.pos >= len(p.s) {
				return nil, false
			}
			if p.s[p.pos] == '}' {
				p.pos++
				return v, true
			}
			k := p.readName()
			if k == "" {
				return nil, false
			}
			p.skip()
			if p.pos >= len(p.s) || p.s[p.pos] != ':' {
				return nil, false
			}
			p.pos++
			item, ok := p.value()
			if !ok {
				return nil, false
			}
			v.Obj = append(v.Obj, Member{Key: k, Val: item})
		}
	case c == '$':
		p.pos++
		name := p.readName()
		if name == "" {
			return nil, false
		}
		return &V{Kind: 'v', Str: name}, true
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		for p.pos < len(p.s) {
			ch := p.s[p.pos]
			if (ch >= '0' && ch <= '9') || ch == '.' || ch == 'e' || ch == 'E' || ch == '+' || ch == '-' {
				p.pos++
			} else {
				break
			}
		}
		f, err := strconv.ParseFloat(p.s[start:p.pos], 64)
		if err != nil {
			return nil, false
		}
		return numV(f), true
	default:
		name := p.readName()
		if name == "" {
			return nil, false
		}
		switch name {
		case "true":
			return boolV(true), true
		case "false":
			return boolV(false), true
		case "null":
			return nullV(), true
		default:
			// Enum: treated as a string.
			return strV(name), true
		}
	}
}

// gqlString parses a GraphQL string literal. No surrogate-pair
// handling: each \uXXXX decodes to one rune.
func (p *gqlParser) gqlString() (string, bool) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), true
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.s) {
				return "", false
			}
			e := p.s[p.pos]
			p.pos++
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.pos+4 > len(p.s) {
					return "", false
				}
				n := 0
				for i := 0; i < 4; i++ {
					ch := p.s[p.pos+i]
					n <<= 4
					switch {
					case ch >= '0' && ch <= '9':
						n |= int(ch - '0')
					case ch >= 'a' && ch <= 'f':
						n |= int(ch-'a') + 10
					case ch >= 'A' && ch <= 'F':
						n |= int(ch-'A') + 10
					default:
						return "", false
					}
				}
				p.pos += 4
				b.WriteRune(rune(n))
			default:
				return "", false
			}
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", false
}

// ---- executor ----

type nodeRef struct {
	typ string
	val interface{}
}

type gqlCtx struct {
	store     *Store
	variables *V
	errors    []string
}

// issueConn is a resolved issues connection.
type issueConn struct {
	repo    *Repo
	items   []*Issue
	hasNext bool
	endCur  string // "" means null endCursor
}

// prConn is a resolved pull-request connection.
type prConn struct {
	repo    *Repo
	items   []*PullRequest
	hasNext bool
}

// filesConn is a resolved changed-files connection.
type filesConn struct {
	diffs []FileDiff
}

// pageInfoVal is a resolved PageInfo.
type pageInfoVal struct {
	hasNext bool
	endCur  string // "" means null
}

// typeVal is a resolved __Type node.
type typeVal struct {
	name     string
	kind     string
	fields   []string
	enumVals []string
}

// payloadVal is a mutation payload: child name -> value.
type payloadVal struct {
	children map[string]interface{}
}

// commentEdgeVal wraps an IssueComment node.
type commentEdgeVal struct {
	comment *Comment
	repo    *Repo
	issue   *Issue
}

// resolveArg returns an argument value with variables substituted.
func (c *gqlCtx) resolveArg(f *gqlField, name string) *V {
	raw, ok := f.args[name]
	if !ok {
		return nullV()
	}
	return c.subst(raw)
}

func (c *gqlCtx) subst(v *V) *V {
	if v == nil {
		return nullV()
	}
	if v.Kind == 'v' {
		if c.variables != nil {
			if got := c.variables.Get(v.Str); got != nil {
				return got
			}
		}
		return nullV()
	}
	if v.Kind == 'a' {
		out := arrV()
		for _, item := range v.Arr {
			out.Arr = append(out.Arr, c.subst(item))
		}
		return out
	}
	if v.Kind == 'o' {
		out := objV()
		for _, m := range v.Obj {
			out.Obj = append(out.Obj, Member{Key: m.Key, Val: c.subst(m.Val)})
		}
		return out
	}
	return v
}

func argInt(v *V, def int) int {
	if v != nil && v.Kind == 'n' {
		return int(v.Num)
	}
	return def
}

func argStr(v *V, def string) string {
	if v != nil && v.Kind == 's' {
		return v.Str
	}
	return def
}

// execSelection executes a selection set against a node, emitting
// fields in query order.
func (c *gqlCtx) execSelection(typ string, val interface{}, fields []*gqlField) *V {
	out := objV()
	for _, f := range fields {
		if f.fragType != "" {
			if f.fragType == typ {
				sub := c.execSelection(typ, val, f.children)
				out.Obj = append(out.Obj, sub.Obj...)
			}
			continue
		}
		out.Obj = append(out.Obj, Member{Key: f.alias, Val: c.execField(typ, val, f)})
	}
	return out
}

// execField resolves one field and renders its value.
func (c *gqlCtx) execField(typ string, val interface{}, f *gqlField) *V {
	if f.name == "__typename" {
		return strV(typ)
	}
	resolved, known := c.resolve(typ, val, f)
	if !known {
		c.errors = append(c.errors, `Cannot query field "`+f.name+`" on type "`+typ+`".`)
		return nullV()
	}
	return c.render(resolved, f.children)
}

// render turns a resolved value into JSON, recursing into nodes.
func (c *gqlCtx) render(v interface{}, children []*gqlField) *V {
	switch t := v.(type) {
	case nil:
		return nullV()
	case *V:
		return t
	case nodeRef:
		if len(children) == 0 {
			return objV()
		}
		return c.execSelection(t.typ, t.val, children)
	case []nodeRef:
		out := arrV()
		for _, nr := range t {
			out.Arr = append(out.Arr, c.render(nr, children))
		}
		return out
	case string:
		return strV(t)
	case int64:
		return numV(float64(t))
	case bool:
		return boolV(t)
	}
	return nullV()
}

// resolve returns the raw value for a field, or known=false for
// an unknown field.
func (c *gqlCtx) resolve(typ string, val interface{}, f *gqlField) (interface{}, bool) {
	switch typ {
	case "Query":
		return c.resolveQuery(f)
	case "Mutation":
		return c.resolveMutation(f)
	case "User":
		switch f.name {
		case "login":
			return viewerLogin, true
		case "name":
			return viewerName, true
		case "id":
			return int64(viewerID), true
		}
	case "Repository":
		return c.resolveRepo(val.(*Repo), f)
	case "Ref":
		if f.name == "name" {
			return "main", true
		}
	case "Issue":
		is := val.(*Issue)
		switch f.name {
		case "id":
			return isID(c, is), true
		case "number":
			return is.Number, true
		case "title":
			return is.Title, true
		case "state":
			if is.State == "closed" {
				return "CLOSED", true
			}
			return "OPEN", true
		case "body":
			return is.Body, true
		}
	case "PullRequest":
		pr := val.(*PullRequest)
		repo := c.repoOf(pr)
		switch f.name {
		case "id":
			return prID(c, pr), true
		case "number":
			return pr.Number, true
		case "title":
			return pr.Title, true
		case "state":
			if pr.Merged {
				return "MERGED", true
			}
			if pr.State == "closed" {
				return "CLOSED", true
			}
			return "OPEN", true
		case "merged":
			return pr.Merged, true
		case "headRefName":
			return pr.HeadRef, true
		case "baseRefName":
			return pr.BaseRef, true
		case "mergeable":
			if pr.State != "open" {
				return "UNKNOWN", true
			}
			if repo != nil && repo.prMergeable(pr) {
				return "MERGEABLE", true
			}
			return "CONFLICTING", true
		case "files":
			first := argInt(c.resolveArg(f, "first"), -1)
			var diffs []FileDiff
			if repo != nil {
				diffs = prDiff(repo, pr)
			}
			if diffs == nil {
				diffs = []FileDiff{}
			}
			if first >= 0 && len(diffs) > first {
				diffs = diffs[:first]
			}
			return nodeRef{typ: "PullRequestChangedFileConnection", val: filesConn{diffs: diffs}}, true
		}
	case "IssueConnection":
		ic := val.(issueConn)
		switch f.name {
		case "nodes":
			var nodes []nodeRef
			for _, is := range ic.items {
				nodes = append(nodes, nodeRef{typ: "Issue", val: is})
			}
			return nodes, true
		case "pageInfo":
			return nodeRef{typ: "PageInfo", val: pageInfoVal{hasNext: ic.hasNext, endCur: ic.endCur}}, true
		case "totalCount":
			return int64(len(ic.items)), true
		}
	case "PullRequestConnection":
		pc := val.(prConn)
		switch f.name {
		case "nodes":
			var nodes []nodeRef
			for _, pr := range pc.items {
				nodes = append(nodes, nodeRef{typ: "PullRequest", val: pr})
			}
			return nodes, true
		case "pageInfo":
			return nodeRef{typ: "PageInfo", val: pageInfoVal{hasNext: pc.hasNext, endCur: ""}}, true
		case "totalCount":
			return int64(len(pc.items)), true
		}
	case "PullRequestChangedFileConnection":
		fc := val.(filesConn)
		switch f.name {
		case "nodes":
			var nodes []nodeRef
			for _, d := range fc.diffs {
				nodes = append(nodes, nodeRef{typ: "PullRequestChangedFile", val: d})
			}
			return nodes, true
		case "pageInfo":
			return nodeRef{typ: "PageInfo", val: pageInfoVal{hasNext: false, endCur: ""}}, true
		}
	case "PullRequestChangedFile":
		d := val.(FileDiff)
		switch f.name {
		case "path", "filename":
			return d.Path, true
		}
	case "PageInfo":
		pi := val.(pageInfoVal)
		switch f.name {
		case "hasNextPage":
			return pi.hasNext, true
		case "endCursor":
			if pi.endCur == "" {
				return nil, true
			}
			return pi.endCur, true
		case "hasPreviousPage":
			return false, true
		case "startCursor":
			return "", true
		}
	case "__Type":
		tv := val.(typeVal)
		switch f.name {
		case "name":
			return tv.name, true
		case "kind":
			return tv.kind, true
		case "fields":
			var nodes []nodeRef
			for _, name := range tv.fields {
				nodes = append(nodes, nodeRef{typ: "__Field", val: name})
			}
			return nodes, true
		case "enumValues":
			var nodes []nodeRef
			for _, name := range tv.enumVals {
				nodes = append(nodes, nodeRef{typ: "__EnumValue", val: name})
			}
			return nodes, true
		}
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return val.(string), true
		}
	case "IssueComment":
		ce := val.(commentEdgeVal)
		switch f.name {
		case "id":
			return globalID("IssueComment", ce.repo.key()+"/"+strconv.FormatInt(ce.issue.Number, 10)+"/"+strconv.FormatInt(ce.comment.ID, 10)), true
		case "body":
			return ce.comment.Body, true
		}
	case "CommentEdge":
		ce := val.(commentEdgeVal)
		if f.name == "node" {
			return nodeRef{typ: "IssueComment", val: ce}, true
		}
	default:
		// Payload types.
		if pv, ok := val.(payloadVal); ok {
			if child, ok := pv.children[f.name]; ok {
				return child, true
			}
			return nil, true
		}
	}
	return nil, false
}

// repoOf finds the repo that owns a PR by scanning the store.
func (c *gqlCtx) repoOf(pr *PullRequest) *Repo {
	for _, key := range c.store.RepoOrd {
		r := c.store.Repos[key]
		for _, p := range r.PRs {
			if p == pr {
				return r
			}
		}
	}
	return nil
}

// issueOf finds the repo that owns an issue.
func (c *gqlCtx) issueRepo(is *Issue) *Repo {
	for _, key := range c.store.RepoOrd {
		r := c.store.Repos[key]
		for _, i := range r.Issues {
			if i == is {
				return r
			}
		}
	}
	return nil
}

func isID(c *gqlCtx, is *Issue) string {
	if r := c.issueRepo(is); r != nil {
		return globalID("Issue", r.key()+"/"+strconv.FormatInt(is.Number, 10))
	}
	return ""
}

func prID(c *gqlCtx, pr *PullRequest) string {
	if r := c.repoOf(pr); r != nil {
		return globalID("PullRequest", r.key()+"/"+strconv.FormatInt(pr.Number, 10))
	}
	return ""
}

func (c *gqlCtx) resolveQuery(f *gqlField) (interface{}, bool) {
	switch f.name {
	case "viewer":
		return nodeRef{typ: "User", val: nil}, true
	case "repository":
		owner := argStr(c.resolveArg(f, "owner"), "")
		name := argStr(c.resolveArg(f, "name"), "")
		if owner != viewerLogin {
			return nil, true
		}
		if repo := c.store.getRepo(owner, name); repo != nil {
			return nodeRef{typ: "Repository", val: repo}, true
		}
		return nil, true
	case "node":
		id := argStr(c.resolveArg(f, "id"), "")
		kind, key, ok := parseGlobalID(id)
		if !ok {
			return nil, true
		}
		switch kind {
		case "Repository":
			parts := strings.SplitN(key, "/", 2)
			if len(parts) == 2 {
				if repo := c.store.getRepo(parts[0], parts[1]); repo != nil {
					return nodeRef{typ: "Repository", val: repo}, true
				}
			}
		case "Issue":
			parts := strings.Split(key, "/")
			if len(parts) == 3 {
				if repo := c.store.getRepo(parts[0], parts[1]); repo != nil {
					if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
						if is := repo.findIssue(n); is != nil {
							return nodeRef{typ: "Issue", val: is}, true
						}
					}
				}
			}
		case "PullRequest":
			parts := strings.Split(key, "/")
			if len(parts) == 3 {
				if repo := c.store.getRepo(parts[0], parts[1]); repo != nil {
					if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
						if pr := repo.findPR(n); pr != nil {
							return nodeRef{typ: "PullRequest", val: pr}, true
						}
					}
				}
			}
		}
		return nil, true
	case "__type":
		name := argStr(c.resolveArg(f, "name"), "")
		tv, ok := typeInfo(name)
		if !ok {
			return nil, true
		}
		return nodeRef{typ: "__Type", val: tv}, true
	}
	return nil, false
}

// typeInfo returns the introspection record for a known type name.
func typeInfo(name string) (typeVal, bool) {
	switch name {
	case "Issue":
		return typeVal{name: name, kind: "OBJECT", fields: []string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "closedAt", "author", "comments", "labels", "repository", "url"}}, true
	case "PullRequest":
		return typeVal{name: name, kind: "OBJECT", fields: []string{"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt", "headRef", "baseRef"}}, true
	case "Repository":
		return typeVal{name: name, kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "updatedAt"}}, true
	case "SearchType":
		return typeVal{name: name, kind: "ENUM", enumVals: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT", "PULL_REQUEST"}}, true
	case "User", "Query", "Mutation":
		return typeVal{name: name, kind: "OBJECT"}, true
	}
	return typeVal{}, false
}

func (c *gqlCtx) resolveRepo(repo *Repo, f *gqlField) (interface{}, bool) {
	switch f.name {
	case "id":
		return globalID("Repository", repo.key()), true
	case "databaseId":
		return repo.ID, true
	case "name":
		return repo.Name, true
	case "nameWithOwner":
		return repo.key(), true
	case "owner":
		return nodeRef{typ: "User", val: nil}, true
	case "defaultBranchRef":
		return nodeRef{typ: "Ref", val: nil}, true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "issue":
		n := int64(argInt(c.resolveArg(f, "number"), 0))
		if is := repo.findIssue(n); is != nil {
			return nodeRef{typ: "Issue", val: is}, true
		}
		return nil, true
	case "issues":
		return nodeRef{typ: "IssueConnection", val: c.issuesConn(repo, f)}, true
	case "pullRequest":
		n := int64(argInt(c.resolveArg(f, "number"), 0))
		if pr := repo.findPR(n); pr != nil {
			return nodeRef{typ: "PullRequest", val: pr}, true
		}
		return nil, true
	case "pullRequests":
		first := argInt(c.resolveArg(f, "first"), -1)
		items := append([]*PullRequest(nil), repo.PRs...)
		hasNext := false
		if first >= 0 && len(items) > first {
			hasNext = true
			items = items[:first]
		}
		return nodeRef{typ: "PullRequestConnection", val: prConn{repo: repo, items: items, hasNext: hasNext}}, true
	}
	return nil, false
}

// issuesConn builds an IssueConnection for repository.issues.
func (c *gqlCtx) issuesConn(repo *Repo, f *gqlField) issueConn {
	// State filter.
	var states map[string]bool
	if st := c.resolveArg(f, "states"); st != nil && st.Kind == 'a' {
		states = map[string]bool{}
		for _, item := range st.Arr {
			if item.Kind == 's' {
				states[item.Str] = true
			}
		}
	}
	desc := false
	if ob := c.resolveArg(f, "orderBy"); ob != nil && ob.Kind == 'o' {
		if d := ob.StrField("direction", ""); d == "DESC" {
			desc = true
		}
	}
	var items []*Issue
	for _, is := range repo.Issues {
		if states != nil && len(states) > 0 {
			want := "OPEN"
			if is.State == "closed" {
				want = "CLOSED"
			}
			if !states[want] {
				continue
			}
		}
		items = append(items, is)
	}
	if desc {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	// after cursor.
	if after := c.resolveArg(f, "after"); after != nil && after.Kind == 's' && after.Str != "" {
		if n, ok := parseCursor(after.Str); ok {
			var kept []*Issue
			for _, is := range items {
				if desc {
					if is.Number < n {
						kept = append(kept, is)
					}
				} else if is.Number > n {
					kept = append(kept, is)
				}
			}
			items = kept
		}
	}
	first := argInt(c.resolveArg(f, "first"), -1)
	hasNext := false
	if first >= 0 && len(items) > first {
		hasNext = true
		items = items[:first]
	}
	endCur := ""
	if len(items) > 0 {
		endCur = cursorFor(items[len(items)-1].Number)
	}
	return issueConn{repo: repo, items: items, hasNext: hasNext, endCur: endCur}
}

// ---- mutations ----

// repoFromGlobalID decodes a Repository global id to a repo.
func (c *gqlCtx) repoFromGlobalID(id string) *Repo {
	kind, key, ok := parseGlobalID(id)
	if !ok || kind != "Repository" {
		return nil
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return c.store.getRepo(parts[0], parts[1])
}

func (c *gqlCtx) resolveMutation(f *gqlField) (interface{}, bool) {
	input := c.resolveArg(f, "input")
	if input == nil || input.Kind != 'o' {
		input = objV()
	}
	switch f.name {
	case "createIssue":
		repo := c.repoFromGlobalID(input.StrField("repositoryId", ""))
		if repo == nil {
			c.errors = append(c.errors, "Repository not found")
			return nodeRef{typ: "CreateIssuePayload", val: payloadVal{children: map[string]interface{}{"issue": nil}}}, true
		}
		is := &Issue{
			Number: repo.NextNum,
			ID:     c.store.NextObjID,
			Title:  input.StrField("title", ""),
			Body:   input.StrField("body", ""),
			State:  "open",
		}
		repo.NextNum++
		c.store.NextObjID++
		repo.Issues = append(repo.Issues, is)
		repo.bump()
		return nodeRef{typ: "CreateIssuePayload", val: payloadVal{children: map[string]interface{}{"issue": nodeRef{typ: "Issue", val: is}}}}, true
	case "closeIssue":
		kind, key, ok := parseGlobalID(input.StrField("issueId", ""))
		var is *Issue
		if ok && kind == "Issue" {
			parts := strings.Split(key, "/")
			if len(parts) == 3 {
				if repo := c.store.getRepo(parts[0], parts[1]); repo != nil {
					if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
						is = repo.findIssue(n)
					}
				}
			}
		}
		if is == nil {
			c.errors = append(c.errors, "Issue not found")
			return nodeRef{typ: "CloseIssuePayload", val: payloadVal{children: map[string]interface{}{"issue": nil}}}, true
		}
		is.State = "closed"
		is.cached = ""
		if r := c.issueRepo(is); r != nil {
			r.bump()
		}
		return nodeRef{typ: "CloseIssuePayload", val: payloadVal{children: map[string]interface{}{"issue": nodeRef{typ: "Issue", val: is}}}}, true
	case "createPullRequest":
		repo := c.repoFromGlobalID(input.StrField("repositoryId", ""))
		if repo == nil {
			c.errors = append(c.errors, "Repository not found")
			return nodeRef{typ: "CreatePullRequestPayload", val: payloadVal{children: map[string]interface{}{"pullRequest": nil}}}, true
		}
		pr, errMsg := createPRFromArgs(c.store, repo, input.StrField("headRefName", ""), input.StrField("baseRefName", ""), input.StrField("title", ""), input.StrField("body", ""))
		if errMsg != "" {
			c.errors = append(c.errors, errMsg)
			return nodeRef{typ: "CreatePullRequestPayload", val: payloadVal{children: map[string]interface{}{"pullRequest": nil}}}, true
		}
		return nodeRef{typ: "CreatePullRequestPayload", val: payloadVal{children: map[string]interface{}{"pullRequest": nodeRef{typ: "PullRequest", val: pr}}}}, true
	case "mergePullRequest":
		kind, key, ok := parseGlobalID(input.StrField("pullRequestId", ""))
		var pr *PullRequest
		if ok && kind == "PullRequest" {
			parts := strings.Split(key, "/")
			if len(parts) == 3 {
				if repo := c.store.getRepo(parts[0], parts[1]); repo != nil {
					if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
						pr = repo.findPR(n)
					}
				}
			}
		}
		if pr == nil {
			c.errors = append(c.errors, "Pull request not found")
			return nodeRef{typ: "MergePullRequestPayload", val: payloadVal{children: map[string]interface{}{"pullRequest": nil}}}, true
		}
		if repo := c.repoOf(pr); repo != nil && !pr.Merged {
			// Merge failures are silently ignored.
			repo.tryMerge(pr, false)
		}
		return nodeRef{typ: "MergePullRequestPayload", val: payloadVal{children: map[string]interface{}{"pullRequest": nodeRef{typ: "PullRequest", val: pr}}}}, true
	case "addComment":
		kind, key, ok := parseGlobalID(input.StrField("subjectId", ""))
		var is *Issue
		var repo *Repo
		if ok && kind == "Issue" {
			parts := strings.Split(key, "/")
			if len(parts) == 3 {
				if r := c.store.getRepo(parts[0], parts[1]); r != nil {
					if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
						is = r.findIssue(n)
						repo = r
					}
				}
			}
		}
		if is == nil {
			c.errors = append(c.errors, "Subject not found")
			return nodeRef{typ: "AddCommentPayload", val: payloadVal{children: map[string]interface{}{"commentEdge": nil}}}, true
		}
		cm := &Comment{ID: c.store.NextObjID, Body: input.StrField("body", "")}
		c.store.NextObjID++
		is.Comments = append(is.Comments, cm)
		is.cached = ""
		repo.bump()
		edge := commentEdgeVal{comment: cm, repo: repo, issue: is}
		return nodeRef{typ: "AddCommentPayload", val: payloadVal{children: map[string]interface{}{"commentEdge": nodeRef{typ: "CommentEdge", val: edge}}}}, true
	}
	return nil, false
}

// ---- handler ----

// runGQLDoc executes one GraphQL document body and returns the
// result value ({"data":...} plus "errors" when present).
func runGQLDoc(store *Store, body *V) *V {
	query := body.StrField("query", "")
	var variables *V
	if v := body.Get("variables"); v != nil && v.Kind == 'o' {
		variables = v
	}
	op, ok := parseGQLDoc(query)
	if !ok {
		return objV(
			Member{Key: "data", Val: nullV()},
			Member{Key: "errors", Val: arrV(objV(Member{Key: "message", Val: strV("Syntax Error: could not parse query")}))},
		)
	}
	ctx := &gqlCtx{store: store, variables: variables}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ctx.execSelection(rootType, nil, op.fields)
	result := objV(Member{Key: "data", Val: data})
	if len(ctx.errors) > 0 {
		errs := arrV()
		for _, msg := range ctx.errors {
			errs.Arr = append(errs.Arr, objV(Member{Key: "message", Val: strV(msg)}))
		}
		result.Obj = append(result.Obj, Member{Key: "errors", Val: errs})
	}
	return result
}

// handleGraphQL serves POST /api/graphql (batch arrays supported).
func handleGraphQL(req *request, store *Store) response {
	body, ok := parseBodyJSON(req)
	if !ok {
		return jsonErr(400, "Invalid JSON")
	}
	if body.Kind == 'a' {
		out := arrV()
		for _, item := range body.Arr {
			if item.Kind != 'o' {
				item = objV()
			}
			out.Arr = append(out.Arr, runGQLDoc(store, item))
		}
		return jsonOK(marshalV(out))
	}
	return jsonOK(marshalV(runGQLDoc(store, body)))
}
