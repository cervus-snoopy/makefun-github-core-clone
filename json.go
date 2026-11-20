package main

// Minimal JSON parsing and encoding, matching the wire behaviour
// described in the design brief: compact output, no HTML escaping,
// control characters below 0x20 escaped as \u00XX (lowercase hex),
// non-ASCII bytes emitted raw.

import (
	"strconv"
	"strings"
)

// V is one parsed JSON value.
type V struct {
	Kind byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'u' null
	Str  string
	Num  float64
	Bool bool
	Arr  []*V
	Obj  []Member
}

// Member is one key/value pair of a JSON object, in document order.
type Member struct {
	Key string
	Val *V
}

func nullV() *V           { return &V{Kind: 'u'} }
func strV(s string) *V    { return &V{Kind: 's', Str: s} }
func numV(f float64) *V   { return &V{Kind: 'n', Num: f} }
func boolV(b bool) *V     { return &V{Kind: 'b', Bool: b} }
func arrV(items ...*V) *V { return &V{Kind: 'a', Arr: items} }
func objV(m ...Member) *V { return &V{Kind: 'o', Obj: m} }

// Get returns the value for key, or nil when the value is not an
// object or the key is absent.
func (v *V) Get(key string) *V {
	if v == nil || v.Kind != 'o' {
		return nil
	}
	for _, m := range v.Obj {
		if m.Key == key {
			return m.Val
		}
	}
	return nil
}

// StrField reads a string field, returning def for absent, null or
// non-string values.
func (v *V) StrField(key, def string) string {
	f := v.Get(key)
	if f != nil && f.Kind == 's' {
		return f.Str
	}
	return def
}

// BoolField reads a boolean field, returning def otherwise.
func (v *V) BoolField(key string, def bool) bool {
	f := v.Get(key)
	if f != nil && f.Kind == 'b' {
		return f.Bool
	}
	return def
}

// NumField reads a numeric field, returning def otherwise.
func (v *V) NumField(key string, def float64) float64 {
	f := v.Get(key)
	if f != nil && f.Kind == 'n' {
		return f.Num
	}
	return def
}

// ArrField returns the array value of a field, or nil.
func (v *V) ArrField(key string) []*V {
	f := v.Get(key)
	if f != nil && f.Kind == 'a' {
		return f.Arr
	}
	return nil
}

// Has reports whether key is present (even with a null value).
func (v *V) Has(key string) bool { return v.Get(key) != nil }

// ---- parsing ----

type jsonParser struct {
	s   string
	pos int
	err bool
}

func parseJSON(s string) (*V, bool) {
	p := &jsonParser{s: s}
	p.skipWS()
	v := p.value()
	if p.err {
		return nil, false
	}
	p.skipWS()
	if p.pos != len(p.s) {
		return nil, false
	}
	return v, true
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.s) {
		switch p.s[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) value() *V {
	if p.pos >= len(p.s) {
		p.err = true
		return nil
	}
	switch c := p.s[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.str()
		if !ok {
			p.err = true
			return nil
		}
		return strV(s)
	case c == 't':
		if strings.HasPrefix(p.s[p.pos:], "true") {
			p.pos += 4
			return boolV(true)
		}
	case c == 'f':
		if strings.HasPrefix(p.s[p.pos:], "false") {
			p.pos += 5
			return boolV(false)
		}
	case c == 'n':
		if strings.HasPrefix(p.s[p.pos:], "null") {
			p.pos += 4
			return nullV()
		}
	default:
		return p.number()
	}
	p.err = true
	return nil
}

func (p *jsonParser) object() *V {
	p.pos++ // {
	v := objV()
	p.skipWS()
	if p.pos < len(p.s) && p.s[p.pos] == '}' {
		p.pos++
		return v
	}
	for {
		p.skipWS()
		if p.pos >= len(p.s) || p.s[p.pos] != '"' {
			p.err = true
			return nil
		}
		k, ok := p.str()
		if !ok {
			p.err = true
			return nil
		}
		p.skipWS()
		if p.pos >= len(p.s) || p.s[p.pos] != ':' {
			p.err = true
			return nil
		}
		p.pos++
		p.skipWS()
		val := p.value()
		if p.err {
			return nil
		}
		// Duplicate keys: last wins.
		replaced := false
		for i := range v.Obj {
			if v.Obj[i].Key == k {
				v.Obj[i].Val = val
				replaced = true
			}
		}
		if !replaced {
			v.Obj = append(v.Obj, Member{Key: k, Val: val})
		}
		p.skipWS()
		if p.pos >= len(p.s) {
			p.err = true
			return nil
		}
		if p.s[p.pos] == '}' {
			p.pos++
			return v
		}
		if p.s[p.pos] != ',' {
			p.err = true
			return nil
		}
		p.pos++
	}
}

func (p *jsonParser) array() *V {
	p.pos++ // [
	v := arrV()
	p.skipWS()
	if p.pos < len(p.s) && p.s[p.pos] == ']' {
		p.pos++
		return v
	}
	for {
		p.skipWS()
		item := p.value()
		if p.err {
			return nil
		}
		v.Arr = append(v.Arr, item)
		p.skipWS()
		if p.pos >= len(p.s) {
			p.err = true
			return nil
		}
		if p.s[p.pos] == ']' {
			p.pos++
			return v
		}
		if p.s[p.pos] != ',' {
			p.err = true
			return nil
		}
		p.pos++
	}
}

func (p *jsonParser) number() *V {
	start := p.pos
	if p.pos < len(p.s) && p.s[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		p.err = true
		return nil
	}
	f, err := strconv.ParseFloat(p.s[start:p.pos], 64)
	if err != nil {
		p.err = true
		return nil
	}
	return numV(f)
}

func (p *jsonParser) hex4() (int, bool) {
	if p.pos+4 > len(p.s) {
		return 0, false
	}
	n := 0
	for i := 0; i < 4; i++ {
		c := p.s[p.pos+i]
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	p.pos += 4
	return n, true
}

// str parses a JSON string starting at the opening quote.
func (p *jsonParser) str() (string, bool) {
	// p.s[p.pos] == '"'
	p.pos++
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
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				cp, ok := p.hex4()
				if !ok {
					return "", false
				}
				// Surrogate pair combining.
				if cp >= 0xD800 && cp <= 0xDBFF && p.pos+6 <= len(p.s) &&
					p.s[p.pos] == '\\' && p.s[p.pos+1] == 'u' {
					save := p.pos
					p.pos += 2
					lo, ok2 := p.hex4()
					if ok2 && lo >= 0xDC00 && lo <= 0xDFFF {
						cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00)
					} else {
						p.pos = save
					}
				}
				b.WriteRune(rune(cp))
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

// ---- encoding ----

const hexLow = "0123456789abcdef"

// jstr returns s as a JSON string literal, escaped per the brief:
// " \ \n \r \t escaped; other bytes < 0x20 as \u00XX (lowercase);
// bytes >= 0x20 emitted raw, including multi-byte UTF-8.
func jstr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				b.WriteString("\\u00")
				b.WriteByte(hexLow[c>>4])
				b.WriteByte(hexLow[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// numStr formats a float64 as JSON: integral values print without
// a decimal point, others shortest round-trip.
func numStr(f float64) string {
	if f == float64(int64(f)) && f >= -9e15 && f <= 9e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// writeV appends the compact JSON encoding of v to sb.
func writeV(sb *strings.Builder, v *V) {
	if v == nil {
		sb.WriteString("null")
		return
	}
	switch v.Kind {
	case 'u':
		sb.WriteString("null")
	case 'b':
		if v.Bool {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case 'n':
		sb.WriteString(numStr(v.Num))
	case 's':
		sb.WriteString(jstr(v.Str))
	case 'a':
		sb.WriteByte('[')
		for i, item := range v.Arr {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeV(sb, item)
		}
		sb.WriteByte(']')
	case 'o':
		sb.WriteByte('{')
		for i, m := range v.Obj {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(jstr(m.Key))
			sb.WriteByte(':')
			writeV(sb, m.Val)
		}
		sb.WriteByte('}')
	}
}

// marshalV renders a parsed value back to compact JSON.
func marshalV(v *V) string {
	var sb strings.Builder
	writeV(&sb, v)
	return sb.String()
}
