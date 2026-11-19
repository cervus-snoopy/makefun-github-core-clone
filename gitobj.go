// Git object model: SHA-1 ids, commits/trees/blobs/tags, pack read/write,
// pkt-line framing, reachability, merge-base, three-way merge.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
	objOfs    = 6
	objRef    = 7
)

const zeroOID = "0000000000000000000000000000000000000000"

type gitObject struct {
	typ  byte
	data []byte
}

func sha1Hex(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func oidBytes(oid string) []byte {
	b, _ := hex.DecodeString(oid)
	return b
}

func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- commits

type commitInfo struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) *commitInfo {
	c := &commitInfo{}
	rest := data
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return c
		}
		line := string(rest[:i])
		rest = rest[i+1:]
		if line == "" {
			c.message = string(rest)
			return c
		}
		switch {
		case strings.HasPrefix(line, "tree "):
			c.tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.parents = append(c.parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.committer = line[10:]
		}
	}
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var sb strings.Builder
	sb.WriteString("tree ")
	sb.WriteString(tree)
	sb.WriteByte('\n')
	for _, p := range parents {
		sb.WriteString("parent ")
		sb.WriteString(p)
		sb.WriteByte('\n')
	}
	sb.WriteString("author ")
	sb.WriteString(author)
	sb.WriteString("\ncommitter ")
	sb.WriteString(committer)
	sb.WriteString("\n\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

func parseTag(data []byte) (target, typ, name, tagger, message string) {
	rest := data
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return
		}
		line := string(rest[:i])
		rest = rest[i+1:]
		if line == "" {
			message = string(rest)
			return
		}
		switch {
		case strings.HasPrefix(line, "object "):
			target = line[7:]
		case strings.HasPrefix(line, "type "):
			typ = line[5:]
		case strings.HasPrefix(line, "tag "):
			name = line[4:]
		case strings.HasPrefix(line, "tagger "):
			tagger = line[7:]
		}
	}
}

func buildTag(target, typ, name, tagger, message string) []byte {
	var sb strings.Builder
	sb.WriteString("object ")
	sb.WriteString(target)
	sb.WriteString("\ntype ")
	sb.WriteString(typ)
	sb.WriteString("\ntag ")
	sb.WriteString(name)
	sb.WriteString("\ntagger ")
	sb.WriteString(tagger)
	sb.WriteString("\n\n")
	sb.WriteString(message)
	return []byte(sb.String())
}

// ---------------------------------------------------------------- trees

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
	}
	return out
}

// treeSortLess orders entries the way git does: directories compare as name+"/".
func treeSortLess(a, b treeEntry) bool {
	an, bn := a.name, b.name
	if a.mode == "40000" {
		an += "/"
	}
	if b.mode == "40000" {
		bn += "/"
	}
	return an < bn
}

func buildTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return treeSortLess(sorted[i], sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(oidBytes(e.oid))
	}
	return buf.Bytes()
}

type treeLeaf struct {
	oid  string
	mode string // "100644" file, "40000" dir never stored as leaf
}

// flattenTree walks a tree into path -> leaf (files only).
func flattenTree(get func(string) *gitObject, treeOID string) map[string]treeLeaf {
	out := make(map[string]treeLeaf)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o := get(oid)
		if o == nil || o.typ != objTree {
			return
		}
		for _, e := range parseTree(o.data) {
			p := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, p+"/")
			} else {
				out[p] = treeLeaf{oid: e.oid, mode: e.mode}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// buildTreeFromFlat builds (and stores) nested trees for a flat path map.
func buildTreeFromFlat(store func(oid string, o *gitObject), flat map[string]treeLeaf) string {
	// Group leaves by directory.
	type dir struct {
		files map[string]treeLeaf
		subs  map[string]*dir
	}
	root := &dir{files: map[string]treeLeaf{}, subs: map[string]*dir{}}
	for path, leaf := range flat {
		parts := strings.Split(path, "/")
		d := root
		for _, p := range parts[:len(parts)-1] {
			sub, ok := d.subs[p]
			if !ok {
				sub = &dir{files: map[string]treeLeaf{}, subs: map[string]*dir{}}
				d.subs[p] = sub
			}
			d = sub
		}
		d.files[parts[len(parts)-1]] = leaf
	}
	var build func(d *dir) string
	build = func(d *dir) string {
		var entries []treeEntry
		for name, leaf := range d.files {
			entries = append(entries, treeEntry{mode: leaf.mode, name: name, oid: leaf.oid})
		}
		for name, sub := range d.subs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(sub)})
		}
		data := buildTree(entries)
		oid := sha1Hex("tree", data)
		store(oid, &gitObject{typ: objTree, data: data})
		return oid
	}
	return build(root)
}

// ---------------------------------------------------------------- reachability

// collectReachable returns the set of object oids reachable from roots.
func collectReachable(get func(string) *gitObject, roots []string) map[string]bool {
	seen := make(map[string]bool)
	var stack []string
	stack = append(stack, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		o := get(oid)
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.typ {
		case objCommit:
			c := parseCommit(o.data)
			stack = append(stack, c.tree)
			stack = append(stack, c.parents...)
		case objTree:
			for _, e := range parseTree(o.data) {
				stack = append(stack, e.oid)
			}
		case objTag:
			target, _, _, _, _ := parseTag(o.data)
			stack = append(stack, target)
		}
	}
	return seen
}

// collectCommits lists commits reachable from tips, tip-first (DFS, parents after).
func collectCommits(get func(string) *gitObject, tips []string) []string {
	var out []string
	seen := make(map[string]bool)
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || oid == zeroOID || seen[oid] {
			return
		}
		o := get(oid)
		if o == nil {
			return
		}
		if o.typ == objTag {
			target, _, _, _, _ := parseTag(o.data)
			visit(target)
			return
		}
		if o.typ != objCommit {
			return
		}
		seen[oid] = true
		out = append(out, oid)
		c := parseCommit(o.data)
		for _, p := range c.parents {
			visit(p)
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return out
}

func isAncestor(get func(string) *gitObject, anc, desc string) bool {
	if anc == desc {
		return true
	}
	if anc == "" || anc == zeroOID {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{desc}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == anc {
			return true
		}
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		o := get(oid)
		if o == nil {
			continue
		}
		if o.typ == objTag {
			target, _, _, _, _ := parseTag(o.data)
			stack = append(stack, target)
			continue
		}
		if o.typ != objCommit {
			continue
		}
		c := parseCommit(o.data)
		stack = append(stack, c.parents...)
	}
	return false
}

func mergeBase(get func(string) *gitObject, a, b string) string {
	ancA := make(map[string]bool)
	stack := []string{a}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || ancA[oid] {
			continue
		}
		ancA[oid] = true
		o := get(oid)
		if o == nil || o.typ != objCommit {
			continue
		}
		stack = append(stack, parseCommit(o.data).parents...)
	}
	// BFS from b: first common ancestor wins.
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		if ancA[oid] {
			return oid
		}
		o := get(oid)
		if o == nil || o.typ != objCommit {
			continue
		}
		queue = append(queue, parseCommit(o.data).parents...)
	}
	return ""
}

// ---------------------------------------------------------------- diff / merge

type fileDiff struct {
	path      string
	status    string // added, modified, removed
	additions int
	deletions int
}

func lineCounts(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines between two blobs (multiset difference).
func lineDiff(oldData, newData []byte) (add, del int) {
	count := func(d []byte) map[string]int {
		m := make(map[string]int)
		if len(d) == 0 {
			return m
		}
		for _, l := range bytes.SplitAfter(d, []byte{'\n'}) {
			if len(l) > 0 {
				m[string(l)]++
			}
		}
		return m
	}
	co, cn := count(oldData), count(newData)
	for l, n := range cn {
		if d := n - co[l]; d > 0 {
			add += d
		}
	}
	for l, n := range co {
		if d := n - cn[l]; d > 0 {
			del += d
		}
	}
	return add, del
}

func diffTrees(get func(string) *gitObject, baseTree, headTree string) []fileDiff {
	base := flattenTree(get, baseTree)
	head := flattenTree(get, headTree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var names []string
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []fileDiff
	for _, p := range names {
		b, inB := base[p]
		h, inH := head[p]
		switch {
		case inB && !inH:
			var d fileDiff
			d.path, d.status = p, "removed"
			if o := get(b.oid); o != nil {
				_, d.deletions = lineDiff(o.data, nil)
			}
			out = append(out, d)
		case !inB && inH:
			var d fileDiff
			d.path, d.status = p, "added"
			if o := get(h.oid); o != nil {
				d.additions, _ = lineDiff(nil, o.data)
			}
			out = append(out, d)
		case b.oid != h.oid:
			var d fileDiff
			d.path, d.status = p, "modified"
			var bo, ho []byte
			if o := get(b.oid); o != nil {
				bo = o.data
			}
			if o := get(h.oid); o != nil {
				ho = o.data
			}
			d.additions, d.deletions = lineDiff(bo, ho)
			out = append(out, d)
		}
	}
	return out
}

// threeWayMerge merges theirs into ours over base. Returns merged flat map and
// whether any path conflicted (both sides changed it differently).
func threeWayMerge(get func(string) *gitObject, baseTree, oursTree, theirsTree string) (map[string]treeLeaf, bool) {
	base := flattenTree(get, baseTree)
	ours := flattenTree(get, oursTree)
	theirs := flattenTree(get, theirsTree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	merged := make(map[string]treeLeaf)
	conflict := false
	for p := range paths {
		b, inB := base[p]
		o, inO := ours[p]
		t, inT := theirs[p]
		switch {
		case inO && inT && o.oid == t.oid:
			merged[p] = o
		case inB && inO && !inT && b.oid == o.oid:
			// theirs deleted, ours unchanged -> deleted
		case inB && !inO && inT && b.oid == t.oid:
			// ours deleted, theirs unchanged -> deleted
		case !inB && inO && !inT:
			merged[p] = o
		case !inB && !inO && inT:
			merged[p] = t
		case inB && inO && !inT:
			// ours changed, theirs deleted -> conflict
			conflict = true
			merged[p] = o
		case inB && !inO && inT:
			conflict = true
			merged[p] = t
		case inB && inO && inT && b.oid == o.oid:
			merged[p] = t
		case inB && inO && inT && b.oid == t.oid:
			merged[p] = o
		case inB && inO && inT:
			conflict = true
			merged[p] = o
		default:
			// both deleted or identical absence
		}
	}
	return merged, conflict
}

// ---------------------------------------------------------------- pkt-lines

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func pktText(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pkt struct {
	kind byte // 'd' data, 'f' flush, '1' delim, '2' end
	data []byte
}

// readPktLines parses a full pkt-line stream. Returns false on framing errors.
func readPktLines(b []byte) ([]pkt, bool) {
	var out []pkt
	i := 0
	for i < len(b) {
		if i+4 > len(b) {
			return nil, false
		}
		hdr := string(b[i : i+4])
		if hdr == "0000" {
			out = append(out, pkt{kind: 'f'})
			i += 4
			continue
		}
		if hdr == "0001" {
			out = append(out, pkt{kind: '1'})
			i += 4
			continue
		}
		if hdr == "0002" {
			out = append(out, pkt{kind: '2'})
			i += 4
			continue
		}
		n, err := strconv.ParseInt(hdr, 16, 32)
		if err != nil || n < 4 || i+int(n) > len(b) {
			return nil, false
		}
		out = append(out, pkt{kind: 'd', data: b[i+4 : i+int(n)]})
		i += int(n)
	}
	return out, true
}

// ---------------------------------------------------------------- pack read/write

// readPack parses a packfile. resolve finds objects not in the pack (thin packs).
func readPack(data []byte, resolve func(string) *gitObject) ([]*gitObject, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack magic")
	}
	version := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("bad pack version %d", version)
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	br := bytes.NewReader(data[12:])
	objects := make([]*gitObject, 0, count)
	byOID := make(map[string]*gitObject)
	posToObj := make(map[int64]*gitObject)
	var ofsBases []struct {
		obj *gitObject
		off int64
	}
	for n := 0; n < count; n++ {
		pos, _ := br.Seek(0, 1)
		typ, size, err := readPackHeader(br)
		if err != nil {
			return nil, err
		}
		var obj *gitObject
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			raw, err := inflateExact(br, size)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: byte(typ), data: raw}
		case objOfs:
			off, err := readOfsDelta(br)
			if err != nil {
				return nil, err
			}
			baseOff := pos - int64(off)
			raw, err := inflateExact(br, size)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: objOfs, data: raw}
			ofsBases = append(ofsBases, struct {
				obj *gitObject
				off int64
			}{obj, baseOff})
		case objRef:
			base := make([]byte, 20)
			if _, err := br.Read(base); err != nil {
				return nil, err
			}
			raw, err := inflateExact(br, size)
			if err != nil {
				return nil, err
			}
			baseOID := hex.EncodeToString(base)
			baseObj := byOID[baseOID]
			if baseObj == nil && resolve != nil {
				baseObj = resolve(baseOID)
			}
			if baseObj == nil {
				return nil, fmt.Errorf("ref-delta base %s not found", baseOID)
			}
			applied, err := applyDelta(baseObj.data, raw)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: baseObj.typ, data: applied}
		default:
			return nil, fmt.Errorf("unknown pack type %d", typ)
		}
		objects = append(objects, obj)
		posToObj[pos] = obj
		if obj.typ != objOfs {
			oid := objectOID(obj)
			byOID[oid] = obj
		}
	}
	// Resolve OFS deltas. Bases always precede their deltas in the pack, so
	// resolving in pack order (recursively) applies each delta to the already
	// resolved base.
	var resolveOfs func(o *gitObject) (*gitObject, error)
	resolveOfs = func(o *gitObject) (*gitObject, error) {
		if o.typ != objOfs {
			return o, nil
		}
		for _, ob := range ofsBases {
			if ob.obj == o {
				base := posToObj[ob.off]
				if base == nil {
					return nil, fmt.Errorf("ofs-delta base at %d not found", ob.off)
				}
				rb, err := resolveOfs(base)
				if err != nil {
					return nil, err
				}
				applied, err := applyDelta(rb.data, o.data)
				if err != nil {
					return nil, err
				}
				o.typ = rb.typ
				o.data = applied
				return o, nil
			}
		}
		return nil, fmt.Errorf("ofs-delta base not found")
	}
	for _, ob := range ofsBases {
		if _, err := resolveOfs(ob.obj); err != nil {
			return nil, err
		}
	}
	// Trailer check is skipped: the client verifies it; we trust the header count.
	_ = version
	return objects, nil
}

func objectOID(o *gitObject) string {
	var typ string
	switch o.typ {
	case objCommit:
		typ = "commit"
	case objTree:
		typ = "tree"
	case objBlob:
		typ = "blob"
	case objTag:
		typ = "tag"
	}
	return sha1Hex(typ, o.data)
}

func readPackHeader(br *bytes.Reader) (int, int, error) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	typ := int(b>>4) & 7
	size := int(b & 0x0f)
	shift := 4
	for b&0x80 != 0 {
		b, err = br.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		size |= int(b&0x7f) << shift
		shift += 7
	}
	return typ, size, nil
}

func readOfsDelta(br *bytes.Reader) (int64, error) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, err
	}
	off := int64(b & 0x7f)
	for b&0x80 != 0 {
		b, err = br.ReadByte()
		if err != nil {
			return 0, err
		}
		off = ((off + 1) << 7) | int64(b&0x7f)
	}
	return off, nil
}

// inflateExact inflates exactly size bytes from a zlib stream sitting at br's
// position. br must be a *bytes.Reader (a ByteReader) so the flate reader does
// not over-read past the stream end.
func inflateExact(br *bytes.Reader, size int) ([]byte, error) {
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, size)
	n := 0
	for n < size {
		m, err := zr.Read(out[n:])
		n += m
		if err != nil {
			if m == 0 {
				return nil, err
			}
			break
		}
	}
	if n != size {
		return nil, fmt.Errorf("inflate size mismatch: got %d want %d", n, size)
	}
	return out, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, bool) {
		v := 0
		shift := 0
		for pos < len(delta) {
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, true
			}
		}
		return 0, false
	}
	srcSize, ok := readVarint()
	if !ok || srcSize != len(base) {
		return nil, fmt.Errorf("delta source size mismatch")
	}
	tgtSize, ok := readVarint()
	if !ok {
		return nil, fmt.Errorf("delta target size missing")
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta copy truncated")
					}
					cpOff |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta copy truncated")
					}
					cpSize |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("delta insert truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch: got %d want %d", len(out), tgtSize)
	}
	return out, nil
}

// writePack serializes objects as a plain (no-delta) pack, deflate level 1.
func writePack(objects []*gitObject) []byte {
	sorted := append([]*gitObject(nil), objects...)
	sort.Slice(sorted, func(i, j int) bool { return objectOID(sorted[i]) < objectOID(sorted[j]) })
	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	n := len(sorted)
	buf.WriteByte(byte(n >> 24))
	buf.WriteByte(byte(n >> 16))
	buf.WriteByte(byte(n >> 8))
	buf.WriteByte(byte(n))
	for _, o := range sorted {
		writePackEntry(&buf, o)
	}
	h := sha1.New()
	h.Write(buf.Bytes())
	buf.Write(h.Sum(nil))
	return buf.Bytes()
}

func writePackEntry(buf *bytes.Buffer, o *gitObject) {
	// varint header: type in bits 4-6, size low 4 bits first.
	size := len(o.data)
	first := byte(o.typ<<4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	buf.WriteByte(first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	w, _ := zlib.NewWriterLevel(buf, 1)
	w.Write(o.data)
	w.Close()
}

// sidebandEncode wraps pack bytes in sideband band-1 pkt-lines (<=64k each).
func sidebandEncode(pack []byte) []byte {
	if len(pack) == 0 {
		return nil
	}
	var out []byte
	const chunk = 65515
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 0, end-i+1)
		payload = append(payload, 1)
		payload = append(payload, pack[i:end]...)
		out = append(out, pktLine(payload)...)
	}
	return out
}
