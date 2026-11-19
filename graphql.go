// GraphQL over POST /api/graphql: a small spec parser (operations, variables,
// arguments, aliases, inline fragments) and an executor for the repository /
// issue / pull-request surface the suite exercises.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- argument values

type gqlArg struct {
	kind byte // s string, i int, f float, b bool, z null, e enum, v var, l list, o object
	str  string
	num  float64
	b    bool
	list []*gqlArg
	obj  map[string]*gqlArg
}

func (a *gqlArg) toJval(vars map[string]*jval) *jval {
	if a == nil {
		return &jval{kind: 'z'}
	}
	switch a.kind {
	case 's', 'e':
		return &jval{kind: 's', str: a.str}
	case 'i', 'f':
		return &jval{kind: 'n', num: a.num}
	case 'b':
		return &jval{kind: 'b', b: a.b}
	case 'z':
		return &jval{kind: 'z'}
	case 'v':
		if v, ok := vars[a.str]; ok {
			return v
		}
		return &jval{kind: 'z'}
	case 'l':
		out := &jval{kind: 'a'}
		for _, item := range a.list {
			out.arr = append(out.arr, item.toJval(vars))
		}
		return out
	case 'o':
		out := &jval{kind: 'o', obj: make(map[string]*jval)}
		for k, v := range a.obj {
			out.obj[k] = v.toJval(vars)
		}
		return out
	}
	return &jval{kind: 'z'}
}

// ---------------------------------------------------------------- parser

type gqlField struct {
	alias  string
	name   string
	args   map[string]*gqlArg
	sel    []*gqlField
	fragOn string
}

type gqlDoc struct {
	kind   string // "query" | "mutation"
	fields []*gqlField
}

type gqlParser struct {
	s string
	i int
}

func (p *gqlParser) ws() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == '#' {
			if c == '#' {
				for p.i < len(p.s) && p.s[p.i] != '\n' {
					p.i++
				}
			} else {
				p.i++
			}
		} else {
			return
		}
	}
}

func (p *gqlParser) peek() byte {
	p.ws()
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *gqlParser) take(c byte) bool {
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *gqlParser) expect(c byte) bool {
	if !p.take(c) {
		return false
	}
	return true
}

func (p *gqlParser) name() (string, bool) {
	p.ws()
	start := p.i
	if p.i < len(p.s) && (p.s[p.i] == '_' || (p.s[p.i] >= 'A' && p.s[p.i] <= 'Z') || (p.s[p.i] >= 'a' && p.s[p.i] <= 'z')) {
		p.i++
		for p.i < len(p.s) {
			c := p.s[p.i]
			if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				p.i++
			} else {
				break
			}
		}
		return p.s[start:p.i], true
	}
	return "", false
}

func (p *gqlParser) str() (string, bool) {
	p.ws()
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", false
	}
	p.i++
	var sb strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return sb.String(), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.s) {
				return "", false
			}
			switch p.s[p.i] {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'u':
				if p.i+4 < len(p.s) {
					n, err := strconv.ParseUint(p.s[p.i+1:p.i+5], 16, 32)
					if err != nil {
						return "", false
					}
					sb.WriteRune(rune(n))
					p.i += 4
				}
			default:
				return "", false
			}
			p.i++
			continue
		}
		sb.WriteByte(c)
		p.i++
	}
	return "", false
}

func (p *gqlParser) value() (*gqlArg, bool) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		s, ok := p.str()
		if !ok {
			return nil, false
		}
		return &gqlArg{kind: 's', str: s}, true
	case c == '$':
		p.i++
		n, ok := p.name()
		if !ok {
			return nil, false
		}
		return &gqlArg{kind: 'v', str: n}, true
	case c == '[':
		p.i++
		v := &gqlArg{kind: 'l'}
		for {
			p.ws()
			if p.take(']') {
				return v, true
			}
			item, ok := p.value()
			if !ok {
				return nil, false
			}
			v.list = append(v.list, item)
		}
	case c == '{':
		p.i++
		v := &gqlArg{kind: 'o', obj: make(map[string]*gqlArg)}
		for {
			p.ws()
			if p.take('}') {
				return v, true
			}
			n, ok := p.name()
			if !ok || !p.expect(':') {
				return nil, false
			}
			item, ok := p.value()
			if !ok {
				return nil, false
			}
			v.obj[n] = item
		}
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.i
		if p.s[p.i] == '-' {
			p.i++
		}
		isFloat := false
		for p.i < len(p.s) {
			c := p.s[p.i]
			if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				if c == '.' || c == 'e' || c == 'E' {
					isFloat = true
				}
				p.i++
			} else {
				break
			}
		}
		n, err := strconv.ParseFloat(p.s[start:p.i], 64)
		if err != nil {
			return nil, false
		}
		if isFloat {
			return &gqlArg{kind: 'f', num: n}, true
		}
		return &gqlArg{kind: 'i', num: n}, true
	default:
		n, ok := p.name()
		if !ok {
			return nil, false
		}
		switch n {
		case "true":
			return &gqlArg{kind: 'b', b: true}, true
		case "false":
			return &gqlArg{kind: 'b', b: false}, true
		case "null":
			return &gqlArg{kind: 'z'}, true
		default:
			return &gqlArg{kind: 'e', str: n}, true
		}
	}
}

// skipDirectives consumes @name(args) sequences.
func (p *gqlParser) skipDirectives() bool {
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '@' {
			p.i++
			if _, ok := p.name(); !ok {
				return false
			}
			if p.peek() == '(' {
				if _, ok := p.args(); !ok {
					return false
				}
			}
			continue
		}
		return true
	}
}

func (p *gqlParser) args() (map[string]*gqlArg, bool) {
	out := make(map[string]*gqlArg)
	if !p.expect('(') {
		return nil, false
	}
	for {
		p.ws()
		if p.take(')') {
			return out, true
		}
		n, ok := p.name()
		if !ok || !p.expect(':') {
			return nil, false
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		out[n] = v
	}
}

// skipVarDefs consumes ($x: Type, ...) — types are not needed, just balanced.
func (p *gqlParser) skipVarDefs() bool {
	if p.peek() != '(' {
		return true
	}
	depth := 0
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '(' || c == '[' || c == '{' {
			depth++
		} else if c == ')' || c == ']' || c == '}' {
			depth--
			if depth == 0 {
				p.i++
				return true
			}
		} else if c == '"' {
			if _, ok := p.str(); !ok {
				return false
			}
			continue
		}
		p.i++
	}
	return false
}

func (p *gqlParser) selectionSet() ([]*gqlField, bool) {
	if !p.expect('{') {
		return nil, false
	}
	var fields []*gqlField
	for {
		p.ws()
		if p.take('}') {
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
	p.ws()
	if p.i+2 < len(p.s) && p.s[p.i] == '.' && p.s[p.i+1] == '.' && p.s[p.i+2] == '.' {
		p.i += 3
		f := &gqlField{}
		if n, ok := p.name(); ok && n == "on" {
			tn, ok := p.name()
			if !ok {
				return nil, false
			}
			f.fragOn = tn
		} else {
			// Named fragment spread: not used by the suite; reject.
			return nil, false
		}
		if !p.skipDirectives() {
			return nil, false
		}
		sel, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
		return f, true
	}
	n, ok := p.name()
	if !ok {
		return nil, false
	}
	f := &gqlField{name: n, alias: n}
	if p.peek() == ':' {
		p.i++
		real, ok := p.name()
		if !ok {
			return nil, false
		}
		f.name = real
	}
	if p.peek() == '(' {
		args, ok := p.args()
		if !ok {
			return nil, false
		}
		f.args = args
	}
	if !p.skipDirectives() {
		return nil, false
	}
	if p.peek() == '{' {
		sel, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
	}
	return f, true
}

func parseGraphQLDocument(src string) (*gqlDoc, bool) {
	p := &gqlParser{s: src}
	doc := &gqlDoc{kind: "query"}
	p.ws()
	if p.peek() == '{' {
		sel, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		doc.fields = sel
		return doc, true
	}
	kw, ok := p.name()
	if !ok || (kw != "query" && kw != "mutation" && kw != "subscription") {
		return nil, false
	}
	doc.kind = kw
	if _, ok := p.name(); ok {
		// operation name (optional)
	}
	if !p.skipVarDefs() {
		return nil, false
	}
	if !p.skipDirectives() {
		return nil, false
	}
	sel, ok := p.selectionSet()
	if !ok {
		return nil, false
	}
	doc.fields = sel
	return doc, true
}

// ---------------------------------------------------------------- executor values

type gnode struct {
	kind string
	// entity
	r     *repo
	issue *issue
	pr    *pullRequest
	// connection
	issues  []*issue
	prs     []*pullRequest
	files   []fileDiff
	hasNext bool
	endCur  string
	// introspection
	tn string
	// scalar
	scalar *jval
	// list items
	list []*gnode
	// mutation payload children
	payload map[string]*gnode
	// single file / comment
	file    *fileDiff
	comment *comment
}

func (n *gnode) typeName() string {
	switch n.kind {
	case "query":
		return "Query"
	case "mutation":
		return "Mutation"
	case "user":
		return "User"
	case "repo":
		return "Repository"
	case "issue":
		return "Issue"
	case "pr":
		return "PullRequest"
	case "branchRef":
		return "Ref"
	case "issueConn":
		return "IssueConnection"
	case "prConn":
		return "PullRequestConnection"
	case "fileConn":
		return "PullRequestChangedFileConnection"
	case "file":
		return "PullRequestChangedFile"
	case "pageInfo":
		return "PageInfo"
	case "type":
		return "__Type"
	case "field":
		return "__Field"
	case "enumVal":
		return "__EnumValue"
	case "comment":
		return "IssueComment"
	case "commentEdge":
		return "IssueCommentEdge"
	case "payload":
		return "Payload"
	}
	return "Query"
}

func scalarStr(s string) *gnode  { return &gnode{kind: "scalar", scalar: &jval{kind: 's', str: s}} }
func scalarNum(n float64) *gnode { return &gnode{kind: "scalar", scalar: &jval{kind: 'n', num: n}} }
func scalarBool(b bool) *gnode   { return &gnode{kind: "scalar", scalar: &jval{kind: 'b', b: b}} }

func cursorFor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func cursorNum(c string) (int, bool) {
	b, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[7:])
	if err != nil {
		return 0, false
	}
	return n, true
}
