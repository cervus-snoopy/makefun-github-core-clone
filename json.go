// Minimal JSON: a small recursive-descent parser for request bodies and a
// fast string escaper for hand-built responses. No encoding/json anywhere.
package main

import (
	"strconv"
	"strings"
)

type jval struct {
	kind byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	obj  map[string]*jval
	arr  []*jval
	str  string
	num  float64
	b    bool
}

func (v *jval) get(key string) *jval {
	if v == nil || v.kind != 'o' {
		return nil
	}
	return v.obj[key]
}

func (v *jval) strOr(def string) string {
	if v == nil {
		return def
	}
	if v.kind == 's' {
		return v.str
	}
	return def
}

func (v *jval) boolOr(def bool) bool {
	if v == nil || v.kind != 'b' {
		return def
	}
	return v.b
}

func (v *jval) numOr(def float64) float64 {
	if v == nil || v.kind != 'n' {
		return def
	}
	return v.num
}

func (v *jval) intOr(def int) int {
	return int(v.numOr(float64(def)))
}

type jparser struct {
	b []byte
	i int
}

func parseJSON(b []byte) (*jval, bool) {
	p := &jparser{b: b}
	p.ws()
	v, ok := p.value()
	if !ok {
		return nil, false
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, false
	}
	return v, true
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() (*jval, bool) {
	if p.i >= len(p.b) {
		return nil, false
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		if !ok {
			return nil, false
		}
		return &jval{kind: 's', str: s}, true
	case c == 't':
		if p.lit("true") {
			return &jval{kind: 'b', b: true}, true
		}
	case c == 'f':
		if p.lit("false") {
			return &jval{kind: 'b', b: false}, true
		}
	case c == 'n':
		if p.lit("null") {
			return &jval{kind: 'z'}, true
		}
	default:
		return p.number()
	}
	return nil, false
}

func (p *jparser) lit(s string) bool {
	if p.i+len(s) > len(p.b) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jparser) object() (*jval, bool) {
	v := &jval{kind: 'o', obj: make(map[string]*jval)}
	p.i++ // {
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return v, true
	}
	for {
		p.ws()
		k, ok := p.str()
		if !ok {
			return nil, false
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.ws()
		val, ok := p.value()
		if !ok {
			return nil, false
		}
		v.obj[k] = val
		p.ws()
		if p.i >= len(p.b) {
			return nil, false
		}
		if p.b[p.i] == '}' {
			p.i++
			return v, true
		}
		if p.b[p.i] != ',' {
			return nil, false
		}
		p.i++
	}
}

func (p *jparser) array() (*jval, bool) {
	v := &jval{kind: 'a'}
	p.i++ // [
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return v, true
	}
	for {
		p.ws()
		val, ok := p.value()
		if !ok {
			return nil, false
		}
		v.arr = append(v.arr, val)
		p.ws()
		if p.i >= len(p.b) {
			return nil, false
		}
		if p.b[p.i] == ']' {
			p.i++
			return v, true
		}
		if p.b[p.i] != ',' {
			return nil, false
		}
		p.i++
	}
}

func (p *jparser) str() (string, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	var sb strings.Builder
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return sb.String(), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", false
			}
			switch e := p.b[p.i]; e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.i+4 >= len(p.b) {
					return "", false
				}
				h := string(p.b[p.i+1 : p.i+5])
				n, err := strconv.ParseUint(h, 16, 32)
				if err != nil {
					return "", false
				}
				p.i += 4
				r := rune(n)
				// Surrogate pair.
				if n >= 0xD800 && n <= 0xDBFF && p.i+6 < len(p.b) && p.b[p.i+1] == '\\' && p.b[p.i+2] == 'u' {
					h2 := string(p.b[p.i+3 : p.i+7])
					n2, err2 := strconv.ParseUint(h2, 16, 32)
					if err2 == nil && n2 >= 0xDC00 && n2 <= 0xDFFF {
						r = 0x10000 + (rune(n)-0xD800)*0x400 + (rune(n2) - 0xDC00)
						p.i += 6
					}
				}
				sb.WriteRune(r)
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

func (p *jparser) number() (*jval, bool) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
		} else {
			break
		}
	}
	if p.i == start {
		return nil, false
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, false
	}
	return &jval{kind: 'n', num: f}, true
}

// ---------------------------------------------------------------- JSON output

// jsonEscape appends s as a JSON string literal (with quotes) to sb.
func jsonEscape(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			sb.WriteString(s[start:i])
			switch c {
			case '"':
				sb.WriteString(`\"`)
			case '\\':
				sb.WriteString(`\\`)
			case '\n':
				sb.WriteString(`\n`)
			case '\r':
				sb.WriteString(`\r`)
			case '\t':
				sb.WriteString(`\t`)
			default:
				const hex = "0123456789abcdef"
				sb.WriteString(`\u00`)
				sb.WriteByte(hex[c>>4])
				sb.WriteByte(hex[c&0xf])
			}
			start = i + 1
		}
	}
	sb.WriteString(s[start:])
	sb.WriteByte('"')
}

func jsonString(s string) string {
	var sb strings.Builder
	jsonEscape(&sb, s)
	return sb.String()
}
