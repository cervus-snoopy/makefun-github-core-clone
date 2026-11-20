package main

// Git object model, pack reading/writing, delta application,
// pkt-line framing and sideband encoding. All state lives in
// memory; packs exist only as byte streams on the wire.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

// hashObject returns the SHA-1 object id for type+data.
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Obj is one stored git object.
type Obj struct {
	Type string
	Data []byte
}

// ---- commit parsing / building ----

// CommitInfo is the parsed form of a commit object.
type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string // raw "name <email> ts tz" line
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitInfo {
	var ci CommitInfo
	s := string(data)
	idx := strings.Index(s, "\n\n")
	var head, msg string
	if idx < 0 {
		head, msg = s, ""
	} else {
		head, msg = s[:idx], s[idx+2:]
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			ci.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			ci.Parents = append(ci.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			ci.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			ci.Committer = line[10:]
		}
	}
	ci.Message = msg
	return ci
}

func buildCommit(tree string, parents []string, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + whoLine + "\n")
	b.WriteString("committer " + whoLine + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// TagInfo is the parsed form of an annotated tag object.
type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagInfo {
	var ti TagInfo
	s := string(data)
	idx := strings.Index(s, "\n\n")
	var head, msg string
	if idx < 0 {
		head, msg = s, ""
	} else {
		head, msg = s[:idx], s[idx+2:]
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			ti.Object = line[7:]
		case strings.HasPrefix(line, "type "):
			ti.Type = line[5:]
		case strings.HasPrefix(line, "tag "):
			ti.Tag = line[4:]
		case strings.HasPrefix(line, "tagger "):
			ti.Tagger = line[7:]
		}
	}
	ti.Message = msg
	return ti
}

// ---- tree parsing / building ----

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[pos : pos+sp])
		rest := pos + sp + 1
		nul := bytes.IndexByte(data[rest:], 0)
		if nul < 0 || rest+nul+21 > len(data) {
			break
		}
		name := string(data[rest : rest+nul])
		oid := hex.EncodeToString(data[rest+nul+1 : rest+nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		pos = rest + nul + 21
	}
	return out
}

// treeSortKey implements git's rule: directory names compare as
// name + "/".
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---- who parsing ----

// Who is a parsed author/committer identity line.
type Who struct {
	Name  string
	Email string
	TS    int64
}

// parseWho parses "name <email> ts tz"; without angle brackets the
// whole line is the name and the timestamp is the fixed epoch.
func parseWho(line string) Who {
	lt := strings.Index(line, "<")
	gt := strings.Index(line, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return Who{Name: line, TS: fixedTS}
	}
	name := strings.TrimSpace(line[:lt])
	email := line[lt+1 : gt]
	rest := strings.Fields(line[gt+1:])
	var ts int64
	if len(rest) > 0 {
		ts, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	return Who{Name: name, Email: email, TS: ts}
}

// formatISO renders a unix timestamp as UTC ISO 8601 with a Z suffix.
func formatISO(ts int64) string {
	// Manual civil-from-days conversion to avoid the time package's
	// wall-clock behaviour; ts is always the fixed epoch or 0 in
	// practice, but the conversion is general.
	secs := ts
	days := secs / 86400
	rem := secs % 86400
	if rem < 0 {
		rem += 86400
		days--
	}
	hour := rem / 3600
	min := (rem % 3600) / 60
	sec := rem % 60
	// days since 1970-01-01 -> civil date (Howard Hinnant's algorithm).
	z := days + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, m, d, hour, min, sec)
}

// ---- pkt-lines ----

// pktLine frames one payload as a pkt-line.
func pktLine(payload string) []byte {
	n := len(payload) + 4
	return append([]byte(fmt.Sprintf("%04x", n)), payload...)
}

const pktFlush = "0000"
const pktDelim = "0001"
const pktEnd = "0002"

// pktPacket is one parsed pkt-line: Kind is "data", "flush",
// "delim" or "end".
type pktPacket struct {
	Kind string
	Data []byte
}

// readPktLines parses a whole pkt-line stream. A bad length or a
// length running past the buffer is a framing error.
func readPktLines(buf []byte) ([]pktPacket, error) {
	var out []pktPacket
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, errors.New("truncated pkt-line")
		}
		hdr := string(buf[pos : pos+4])
		switch hdr {
		case pktFlush:
			out = append(out, pktPacket{Kind: "flush"})
			pos += 4
			continue
		case pktDelim:
			out = append(out, pktPacket{Kind: "delim"})
			pos += 4
			continue
		case pktEnd:
			out = append(out, pktPacket{Kind: "end"})
			pos += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || pos+int(n) > len(buf) {
			return nil, errors.New("bad pkt-line length")
		}
		out = append(out, pktPacket{Kind: "data", Data: buf[pos+4 : pos+int(n)]})
		pos += int(n)
	}
	return out, nil
}

// ---- sideband ----

// sidebandEncode wraps raw bytes as band-1 pkt-lines, chunked so
// no pkt-line exceeds 65520 bytes. An empty input yields no output.
func sidebandEncode(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var out []byte
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		chunk := data[:n]
		data = data[n:]
		payload := append([]byte{0x01}, chunk...)
		out = append(out, pktLine(string(payload))...)
	}
	return out
}

// ---- pack reading ----

var packTypeNames = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// packEntry is one decoded pack entry before delta resolution.
type packEntry struct {
	start    int    // byte offset of the entry header in the pack
	typ      int    // pack type code
	size     int64  // declared uncompressed size
	raw      []byte // inflated data for non-delta entries
	delta    []byte // inflated delta data for OFS/REF deltas
	ofsBase  int    // resolved start offset for OFS deltas (-1 if none)
	refBase  string // base OID for REF deltas
	resolved *Obj   // final object once deltas are applied
}

// readPack parses a pack byte stream. resolve looks up an OID in
// the repo (for thin REF_DELTA packs). The trailer is not verified.
func readPack(pack []byte, resolve func(oid string) *Obj) ([]*Obj, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("bad pack magic")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("bad pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	pos := 12
	entries := make([]*packEntry, 0, count)
	byStart := map[int]*packEntry{}
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(pack) {
			return nil, errors.New("truncated pack")
		}
		// Entry header varint.
		b := pack[pos]
		pos++
		typ := int((b >> 4) & 0x7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(pack) {
				return nil, errors.New("truncated pack")
			}
			b = pack[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		e := &packEntry{start: start, typ: typ, size: size, ofsBase: -1}
		switch typ {
		case 6: // OFS_DELTA
			if pos >= len(pack) {
				return nil, errors.New("truncated pack")
			}
			b = pack[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(pack) {
					return nil, errors.New("truncated pack")
				}
				b = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			e.ofsBase = start - int(off)
		case 7: // REF_DELTA
			if pos+20 > len(pack) {
				return nil, errors.New("truncated pack")
			}
			e.refBase = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
		case 1, 2, 3, 4:
			// plain object
		default:
			return nil, fmt.Errorf("unknown pack type %d", typ)
		}
		// Inflate the zlib stream. bytes.Reader implements
		// io.ByteReader, so the zlib reader consumes exactly the
		// stream bytes and the reader position marks its end.
		br := bytes.NewReader(pack[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		_, err = out.ReadFrom(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		consumed := len(pack[pos:]) - br.Len()
		pos += consumed
		if int64(out.Len()) != size {
			return nil, fmt.Errorf("inflate size mismatch: got %d want %d", out.Len(), size)
		}
		if typ == 6 || typ == 7 {
			e.delta = out.Bytes()
		} else {
			e.raw = out.Bytes()
			e.resolved = &Obj{Type: packTypeNames[typ], Data: e.raw}
		}
		entries = append(entries, e)
		byStart[start] = e
	}
	// Resolve deltas recursively, in pack order.
	var resolveEntry func(e *packEntry) (*Obj, error)
	resolveEntry = func(e *packEntry) (*Obj, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		var base *Obj
		if e.typ == 6 {
			be, ok := byStart[e.ofsBase]
			if !ok {
				return nil, fmt.Errorf("ofs-delta base at %d not found", e.ofsBase)
			}
			var err error
			base, err = resolveEntry(be)
			if err != nil {
				return nil, err
			}
		} else {
			for _, pe := range entries {
				if pe.resolved != nil && hashObject(pe.resolved.Type, pe.resolved.Data) == e.refBase {
					base = pe.resolved
					break
				}
			}
			if base == nil && resolve != nil {
				base = resolve(e.refBase)
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s not found", e.refBase)
			}
		}
		data, err := applyDelta(base.Data, e.delta)
		if err != nil {
			return nil, err
		}
		e.resolved = &Obj{Type: base.Type, Data: data}
		return e.resolved, nil
	}
	out := make([]*Obj, 0, len(entries))
	for _, e := range entries {
		obj, err := resolveEntry(e)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

// applyDelta applies a git delta stream to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, bool) {
		var v int64
		var shift uint
		for pos < len(delta) {
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, true
			}
		}
		return 0, false
	}
	srcSize, ok := readVarint()
	if !ok {
		return nil, errors.New("delta source size missing")
	}
	if srcSize != int64(len(base)) {
		return nil, errors.New("delta source size mismatch")
	}
	tgtSize, ok := readVarint()
	if !ok {
		return nil, errors.New("delta target size missing")
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base.
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					off |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(1<<uint(i+4)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					size |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off < 0 || size < 0 || off+size > int64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			// Insert literal bytes.
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch: got %d want %d", len(out), tgtSize)
	}
	return out, nil
}

// ---- pack writing ----

// writePack serialises objects as a plain pack (no deltas):
// version 2, entries sorted by OID ascending, zlib level 1,
// trailer = SHA-1 of all preceding bytes.
func writePack(objs []*Obj) []byte {
	type pair struct {
		oid string
		obj *Obj
	}
	pairs := make([]pair, 0, len(objs))
	for _, o := range objs {
		pairs = append(pairs, pair{oid: hashObject(o.Type, o.Data), obj: o})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].oid < pairs[j].oid })
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(pairs)))
	buf.Write(hdr[:])
	typeCode := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, pr := range pairs {
		code := typeCode[pr.obj.Type]
		size := int64(len(pr.obj.Data))
		// Entry header varint: low 4 bits in byte 0, then 7-bit groups.
		var hdrBytes []byte
		first := byte(code<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		hdrBytes = append(hdrBytes, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			hdrBytes = append(hdrBytes, b)
		}
		buf.Write(hdrBytes)
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, 1)
		zw.Write(pr.obj.Data)
		zw.Close()
		buf.Write(zb.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}
