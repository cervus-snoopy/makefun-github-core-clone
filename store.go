package main

// In-memory store: repos, objects, refs, issues, pull requests,
// labels, generation counters and the response caches. No data
// ever touches disk.

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

const (
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	viewerID    = 1000
	fixedTS     = int64(1735689600)
	fixedISO    = "2025-01-01T00:00:00Z"
)

const whoLine = "ada iversen <adaiversen@example.invalid> 1735689600 +0000"

// Label is a repo label.
type Label struct {
	Name  string
	Color string
}

// Comment is one issue comment.
type Comment struct {
	ID   int64
	Body string
}

// Issue is one repository issue.
type Issue struct {
	Number   int64
	ID       int64
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*Comment
	cached   string // cached JSON serialisation, "" = stale
}

// PullRequest is one repository pull request.
type PullRequest struct {
	Number   int64
	ID       int64
	Title    string
	Body     string
	State    string // "open" | "closed"
	HeadRef  string
	BaseRef  string
	Merged   bool
	MergeSHA string
	MergedAt string
}

// cacheEntry is one cached advertisement / fetch response.
type cacheEntry struct {
	gen  int
	data []byte
}

// Repo is one repository and everything it holds.
type Repo struct {
	Owner       string
	Name        string
	ID          int64
	Description string
	Private     bool

	Objects map[string]*Obj
	Refs    map[string]string
	RefOrd  []string // ref names in creation order

	LabelMap map[string]*Label
	LabelOrd []string
	Issues   []*Issue
	PRs      []*PullRequest
	NextNum  int64 // next issue/PR number, starts at 1
	Gen      int

	advCache      map[string]cacheEntry
	fetchCache    map[string]cacheEntry
	fetchOrder    []string // FIFO insertion order for the fetch cache
	repoJSONCache string   // cached repo JSON, "" = stale
}

func newRepo(owner, name string, id int64) *Repo {
	return &Repo{
		Owner:      owner,
		Name:       name,
		ID:         id,
		Objects:    map[string]*Obj{},
		Refs:       map[string]string{},
		LabelMap:   map[string]*Label{},
		NextNum:    1,
		advCache:   map[string]cacheEntry{},
		fetchCache: map[string]cacheEntry{},
	}
}

// bump records a mutation: generation advances and the cached
// repo JSON is invalidated.
func (r *Repo) bump() {
	r.Gen++
	r.repoJSONCache = ""
}

func (r *Repo) key() string { return r.Owner + "/" + r.Name }

// putObject stores an object, returning its OID. A duplicate
// insert is a no-op.
func (r *Repo) putObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Obj{Type: typ, Data: data}
	}
	return oid
}

// getObject returns the object for oid, or nil.
func (r *Repo) getObject(oid string) *Obj { return r.Objects[oid] }

// setRef creates or updates a ref, remembering creation order.
func (r *Repo) setRef(name, oid string) {
	if _, ok := r.Refs[name]; !ok {
		r.RefOrd = append(r.RefOrd, name)
	}
	r.Refs[name] = oid
	r.bump()
}

// deleteRef removes a ref and its place in the creation order.
func (r *Repo) deleteRef(name string) {
	delete(r.Refs, name)
	for i, n := range r.RefOrd {
		if n == name {
			r.RefOrd = append(r.RefOrd[:i], r.RefOrd[i+1:]...)
			break
		}
	}
	r.bump()
}

// sortedRefNames returns ref names in lexicographic order.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// branchNames returns branch names (refs/heads/*) in insertion order.
func (r *Repo) branchNames() []string {
	var out []string
	for _, n := range r.RefOrd {
		if strings.HasPrefix(n, "refs/heads/") {
			out = append(out, strings.TrimPrefix(n, "refs/heads/"))
		}
	}
	return out
}

// findIssue returns the issue with the given number, or nil.
func (r *Repo) findIssue(n int64) *Issue {
	for _, is := range r.Issues {
		if is.Number == n {
			return is
		}
	}
	return nil
}

// findPR returns the pull request with the given number, or nil.
func (r *Repo) findPR(n int64) *PullRequest {
	for _, pr := range r.PRs {
		if pr.Number == n {
			return pr
		}
	}
	return nil
}

// Store is the global in-memory state.
type Store struct {
	Repos      map[string]*Repo
	RepoOrd    []string // repo keys in creation order
	NextRepoID int64
	NextObjID  int64 // shared counter for issues, PRs and comments
}

func newStore() *Store {
	return &Store{Repos: map[string]*Repo{}, NextRepoID: 5000, NextObjID: 1}
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.Repos[owner+"/"+name]
}

func (s *Store) createRepo(owner, name, desc string, priv bool) *Repo {
	r := newRepo(owner, name, s.NextRepoID)
	s.NextRepoID++
	r.Description = desc
	r.Private = priv
	s.Repos[r.key()] = r
	s.RepoOrd = append(s.RepoOrd, r.key())
	return r
}

func (s *Store) deleteRepo(owner, name string) {
	key := owner + "/" + name
	delete(s.Repos, key)
	for i, k := range s.RepoOrd {
		if k == key {
			s.RepoOrd = append(s.RepoOrd[:i], s.RepoOrd[i+1:]...)
			break
		}
	}
}

// ---- global ids ----

// globalID builds the base64 global id for a kind/key pair.
func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

// parseGlobalID decodes a global id into kind and key.
func parseGlobalID(id string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	i := strings.Index(string(raw), ":")
	if i < 0 {
		return "", "", false
	}
	return string(raw[:i]), string(raw[i+1:]), true
}

// cursorFor builds a pagination cursor for an issue/PR number.
func cursorFor(n int64) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(n, 10)))
}

// parseCursor decodes a cursor, returning the number and whether
// it was well-formed.
func parseCursor(c string) (int64, bool) {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	s := string(raw)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.ParseInt(s[len("cursor:"):], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---- graph algorithms ----

// peelToCommit follows tag objects until a commit (or a
// non-commit object) is reached; "" when the chain is broken.
func (r *Repo) peelToCommit(oid string) string {
	for {
		o := r.getObject(oid)
		if o == nil {
			return ""
		}
		if o.Type == "commit" {
			return oid
		}
		if o.Type == "tag" {
			oid = parseTag(o.Data).Object
			continue
		}
		return ""
	}
}

// collectReachable returns every OID reachable from the roots
// (commits, trees, tags).
func (r *Repo) collectReachable(roots []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		o := r.getObject(oid)
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.Type {
		case "commit":
			ci := parseCommit(o.Data)
			stack = append(stack, ci.Tree)
			stack = append(stack, ci.Parents...)
		case "tree":
			for _, e := range parseTree(o.Data) {
				stack = append(stack, e.OID)
			}
		case "tag":
			stack = append(stack, parseTag(o.Data).Object)
		}
	}
	return seen
}

// collectCommits returns commit OIDs in tip-first DFS order,
// peeling tag tips first.
func (r *Repo) collectCommits(tips []string) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		oid = r.peelToCommit(oid)
		if oid == "" || seen[oid] {
			return
		}
		o := r.getObject(oid)
		if o == nil || o.Type != "commit" {
			return
		}
		seen[oid] = true
		out = append(out, oid)
		for _, p := range parseCommit(o.Data).Parents {
			visit(p)
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return out
}

// isAncestor reports whether anc is an ancestor of desc (or equal
// to it); an empty or zero anc is always an ancestor.
func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == "" || anc == zeroOID {
		return true
	}
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, r.peelToCommit(desc))
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		if oid == anc {
			return true
		}
		o := r.getObject(oid)
		if o == nil || o.Type != "commit" {
			continue
		}
		stack = append(stack, parseCommit(o.Data).Parents...)
	}
	return false
}

// ancestors returns the set of commit ancestors of oid (including
// oid itself when it is a commit).
func (r *Repo) ancestors(oid string) map[string]bool {
	set := map[string]bool{}
	oid = r.peelToCommit(oid)
	var stack []string
	if oid != "" {
		stack = append(stack, oid)
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || set[cur] {
			continue
		}
		o := r.getObject(cur)
		if o == nil || o.Type != "commit" {
			continue
		}
		set[cur] = true
		stack = append(stack, parseCommit(o.Data).Parents...)
	}
	return set
}

// mergeBase returns the first common ancestor of a and b in BFS
// order from b, or "" when there is none.
func (r *Repo) mergeBase(a, b string) string {
	aSet := r.ancestors(a)
	b = r.peelToCommit(b)
	if b == "" {
		return ""
	}
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if aSet[cur] {
			return cur
		}
		o := r.getObject(cur)
		if o == nil || o.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(o.Data).Parents...)
	}
	return ""
}

// FileEntry is one file in a flattened tree.
type FileEntry struct {
	OID  string
	Mode string
}

// treeClosure returns a tree OID plus every object under it
// (subtrees and blobs). It never follows commit parents, so it is
// the right closure for a depth-limited fetch.
func (r *Repo) treeClosure(treeOID string) map[string]bool {
	out := map[string]bool{}
	var stack []string
	if treeOID != "" {
		stack = append(stack, treeOID)
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || out[oid] {
			continue
		}
		o := r.getObject(oid)
		if o == nil {
			continue
		}
		out[oid] = true
		if o.Type == "tree" {
			for _, e := range parseTree(o.Data) {
				stack = append(stack, e.OID)
			}
		}
	}
	return out
}

// flattenTree walks a tree recursively into path -> file entries.
func (r *Repo) flattenTree(treeOID string) map[string]FileEntry {
	out := map[string]FileEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o := r.getObject(oid)
		if o == nil || o.Type != "tree" {
			return
		}
		for _, e := range parseTree(o.Data) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = FileEntry{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// buildTreeFromFlat builds trees bottom-up from a flat file map
// and stores every intermediate tree, returning the root OID.
func (r *Repo) buildTreeFromFlat(flat map[string]FileEntry) string {
	// Group leaves by directory, deepest first.
	dirs := map[string][]TreeEntry{}
	var dirList []string
	seenDir := map[string]bool{}
	addDir := func(d string) {
		if !seenDir[d] {
			seenDir[d] = true
			dirList = append(dirList, d)
		}
	}
	addDir("")
	for path := range flat {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			addDir(strings.Join(parts[:i], "/"))
		}
	}
	for path, fe := range flat {
		dir := ""
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			dir, name = path[:i], path[i+1:]
		}
		dirs[dir] = append(dirs[dir], TreeEntry{Mode: fe.Mode, Name: name, OID: fe.OID})
	}
	// Deepest directories first. Depth is the component count,
	// not the slash count: "" is depth 0 and "docs" is depth 1
	// (both contain zero slashes, so sorting by slashes alone
	// can build a parent before its children and store an empty
	// OID for the child tree).
	dirDepth := func(d string) int {
		if d == "" {
			return 0
		}
		return strings.Count(d, "/") + 1
	}
	sort.Slice(dirList, func(i, j int) bool {
		di, dj := dirDepth(dirList[i]), dirDepth(dirList[j])
		if di != dj {
			return di > dj
		}
		return dirList[i] < dirList[j]
	})
	treeOID := map[string]string{}
	for _, d := range dirList {
		entries := append([]TreeEntry(nil), dirs[d]...)
		// Add subdirectories of d as tree entries.
		for _, sub := range dirList {
			if sub == d || sub == "" {
				continue
			}
			parent := ""
			if i := strings.LastIndex(sub, "/"); i >= 0 {
				parent = sub[:i]
			}
			if parent == d {
				name := sub
				if i := strings.LastIndex(sub, "/"); i >= 0 {
					name = sub[i+1:]
				}
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: treeOID[sub]})
			}
		}
		oid := r.putObject("tree", buildTree(entries))
		treeOID[d] = oid
	}
	return treeOID[""]
}

// splitLines splits blob bytes into lines, keeping the trailing
// newline on each line. An empty blob has no lines.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := string(data)
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// lineDiff counts added/deleted lines between two blobs by
// multiset difference of their lines.
func lineDiff(oldData, newData []byte) (int, int) {
	oldCount := map[string]int{}
	for _, l := range splitLines(oldData) {
		oldCount[l]++
	}
	newCount := map[string]int{}
	for _, l := range splitLines(newData) {
		newCount[l]++
	}
	added, deleted := 0, 0
	for l, n := range newCount {
		if d := n - oldCount[l]; d > 0 {
			added += d
		}
	}
	for l, n := range oldCount {
		if d := n - newCount[l]; d > 0 {
			deleted += d
		}
	}
	return added, deleted
}

// FileDiff is one entry of a tree diff.
type FileDiff struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

// diffTrees diffs two trees (by OID) via their flattened forms.
func (r *Repo) diffTrees(baseTree, headTree string) []FileDiff {
	base := map[string]FileEntry{}
	if baseTree != "" {
		base = r.flattenTree(baseTree)
	}
	head := map[string]FileEntry{}
	if headTree != "" {
		head = r.flattenTree(headTree)
	}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []FileDiff
	for _, p := range sorted {
		b, inBase := base[p]
		h, inHead := head[p]
		switch {
		case !inBase && inHead:
			add, _ := lineDiff(nil, r.blobBytes(h.OID))
			out = append(out, FileDiff{Path: p, Status: "added", Additions: add})
		case inBase && !inHead:
			_, del := lineDiff(r.blobBytes(b.OID), nil)
			out = append(out, FileDiff{Path: p, Status: "removed", Deletions: del})
		case inBase && inHead && b.OID != h.OID:
			add, del := lineDiff(r.blobBytes(b.OID), r.blobBytes(h.OID))
			out = append(out, FileDiff{Path: p, Status: "modified", Additions: add, Deletions: del})
		}
	}
	return out
}

// blobBytes returns a blob's data, or nil when missing/not a blob.
func (r *Repo) blobBytes(oid string) []byte {
	o := r.getObject(oid)
	if o == nil || o.Type != "blob" {
		return nil
	}
	return o.Data
}

// treeOfCommit returns the tree OID of a commit, or "".
func (r *Repo) treeOfCommit(oid string) string {
	o := r.getObject(oid)
	if o == nil || o.Type != "commit" {
		return ""
	}
	return parseCommit(o.Data).Tree
}

// threeWayMerge merges ours and theirs against base. It returns
// the merged flat map and whether any path conflicted.
func (r *Repo) threeWayMerge(baseTree, oursTree, theirsTree string) (map[string]FileEntry, bool) {
	base := map[string]FileEntry{}
	if baseTree != "" {
		base = r.flattenTree(baseTree)
	}
	ours := map[string]FileEntry{}
	if oursTree != "" {
		ours = r.flattenTree(oursTree)
	}
	theirs := map[string]FileEntry{}
	if theirsTree != "" {
		theirs = r.flattenTree(theirsTree)
	}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	merged := map[string]FileEntry{}
	conflict := false
	for p := range paths {
		b, inBase := base[p]
		o, inOurs := ours[p]
		t, inTheirs := theirs[p]
		same := func(a FileEntry, aOK bool, bb FileEntry, bOK bool) bool {
			return aOK == bOK && (!aOK || a.OID == bb.OID)
		}
		switch {
		case same(o, inOurs, t, inTheirs):
			if inOurs {
				merged[p] = o
			}
		case same(o, inOurs, b, inBase):
			// Ours unchanged: take theirs (including deletion).
			if inTheirs {
				merged[p] = t
			}
		case same(t, inTheirs, b, inBase):
			if inOurs {
				merged[p] = o
			}
		case !inOurs && inTheirs:
			// Ours deleted, theirs changed: conflict, keep theirs.
			conflict = true
			merged[p] = t
		case inOurs && !inTheirs:
			// Theirs deleted, ours changed: conflict, keep ours.
			conflict = true
			merged[p] = o
		default:
			// Both changed differently: conflict, keep ours.
			conflict = true
			if inOurs {
				merged[p] = o
			}
		}
	}
	return merged, conflict
}

// resolveRef resolves a ref selector for contents GET: "" means
// the default branch, a branch name means that branch, a raw OID
// means itself (peeled when it is a tag). Returns "" when it
// cannot be resolved to a commit.
func (r *Repo) resolveRef(ref string) string {
	if ref == "" {
		return r.peelToCommit(r.Refs["refs/heads/main"])
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return r.peelToCommit(oid)
	}
	// Raw OID: itself if a commit, peeled target if a tag.
	if o := r.getObject(ref); o != nil {
		if o.Type == "commit" {
			return ref
		}
		if o.Type == "tag" {
			return r.peelToCommit(ref)
		}
	}
	return ""
}

// mergeStatus describes the outcome of a squash-merge attempt.
type mergeStatus int

const (
	mergeOK mergeStatus = iota
	mergeConflict
	mergeNotFound
)

// tryMerge computes the squash merge of a PR without storing
// anything when dry is true; with dry=false it stores the merge
// commit, updates the base branch and closes the PR.
func (r *Repo) tryMerge(pr *PullRequest, dry bool) (string, mergeStatus) {
	if pr == nil || !pr.Merged && pr.State != "open" {
		return "", mergeNotFound
	}
	if pr.Merged {
		return pr.MergeSHA, mergeOK
	}
	headOID, ok := r.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return "", mergeNotFound
	}
	baseOID, ok := r.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return "", mergeNotFound
	}
	headCommit := r.peelToCommit(headOID)
	baseCommit := r.peelToCommit(baseOID)
	if headCommit == "" || baseCommit == "" {
		return "", mergeNotFound
	}
	base := r.mergeBase(baseCommit, headCommit)
	if base == "" {
		return "", mergeNotFound
	}
	merged, conflict := r.threeWayMerge(r.treeOfCommit(base), r.treeOfCommit(baseCommit), r.treeOfCommit(headCommit))
	if conflict {
		return "", mergeConflict
	}
	if dry {
		return "", mergeOK
	}
	treeOID := r.buildTreeFromFlat(merged)
	msg := "Merge pull request #" + strconv.FormatInt(pr.Number, 10) + " from " + r.Owner + "/" + pr.HeadRef + "\n\n" + pr.Title + "\n"
	commitOID := r.putObject("commit", buildCommit(treeOID, []string{baseCommit}, msg))
	r.setRef("refs/heads/"+pr.BaseRef, commitOID)
	pr.State = "closed"
	pr.Merged = true
	pr.MergeSHA = commitOID
	pr.MergedAt = fixedISO
	r.bump()
	return commitOID, mergeOK
}

// prMergeable reports whether a PR could be squash-merged now.
func (r *Repo) prMergeable(pr *PullRequest) bool {
	if pr == nil || pr.State != "open" {
		return false
	}
	_, st := r.tryMerge(pr, true)
	return st == mergeOK
}

// ---- JSON builders for repo / issue / PR ----

// repoJSON builds (and caches) the repository object JSON.
func (r *Repo) repoJSON() string {
	if r.repoJSONCache != "" {
		return r.repoJSONCache
	}
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.FormatInt(r.ID, 10))
	b.WriteString(`,"node_id":`)
	b.WriteString(jstr(globalID("Repository", r.key())))
	b.WriteString(`,"name":`)
	b.WriteString(jstr(r.Name))
	b.WriteString(`,"full_name":`)
	b.WriteString(jstr(r.key()))
	b.WriteString(`,"owner":{"login":`)
	b.WriteString(jstr(r.Owner))
	b.WriteString(`,"id":1000,"type":"User"},"private":`)
	if r.Private {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"description":`)
	if r.Description == "" {
		b.WriteString("null")
	} else {
		b.WriteString(jstr(r.Description))
	}
	b.WriteString(`,"default_branch":"main","html_url":`)
	b.WriteString(jstr("https://github.com/" + r.key()))
	b.WriteString(`,"url":`)
	b.WriteString(jstr("https://api.github.com/repos/" + r.key()))
	b.WriteString(`,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","has_issues":true,"fork":false}`)
	r.repoJSONCache = b.String()
	return r.repoJSONCache
}

// issueJSON builds (and caches) the issue object JSON.
func (is *Issue) issueJSON(r *Repo) string {
	if is.cached != "" {
		return is.cached
	}
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.FormatInt(is.ID, 10))
	b.WriteString(`,"node_id":`)
	b.WriteString(jstr(globalID("Issue", r.key()+"/"+strconv.FormatInt(is.Number, 10))))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.FormatInt(is.Number, 10))
	b.WriteString(`,"title":`)
	b.WriteString(jstr(is.Title))
	b.WriteString(`,"body":`)
	b.WriteString(jstr(is.Body))
	b.WriteString(`,"state":`)
	b.WriteString(jstr(is.State))
	b.WriteString(`,"labels":[`)
	for i, name := range is.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		color := "ededed"
		if lb, ok := r.LabelMap[name]; ok {
			color = lb.Color
		}
		b.WriteString(`{"name":`)
		b.WriteString(jstr(name))
		b.WriteString(`,"color":`)
		b.WriteString(jstr(color))
		b.WriteString(`}`)
	}
	b.WriteString(`],"user":{"login":"adaiversen","id":1000,"type":"User"},"assignee":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","closed_at":`)
	if is.State == "closed" {
		b.WriteString(`"2025-01-01T00:00:00Z"`)
	} else {
		b.WriteString("null")
	}
	b.WriteString(`,"comments":`)
	b.WriteString(strconv.Itoa(len(is.Comments)))
	b.WriteString(`}`)
	is.cached = b.String()
	return is.cached
}

// prJSON builds the pull request object JSON (never cached).
func (pr *PullRequest) prJSON(r *Repo) string {
	headSHA := ""
	if oid, ok := r.Refs["refs/heads/"+pr.HeadRef]; ok {
		headSHA = r.peelToCommit(oid)
		if headSHA == "" {
			headSHA = oid
		}
	}
	baseSHA := ""
	if oid, ok := r.Refs["refs/heads/"+pr.BaseRef]; ok {
		baseSHA = r.peelToCommit(oid)
		if baseSHA == "" {
			baseSHA = oid
		}
	}
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.FormatInt(pr.ID, 10))
	b.WriteString(`,"node_id":`)
	b.WriteString(jstr(globalID("PullRequest", r.key()+"/"+strconv.FormatInt(pr.Number, 10))))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.FormatInt(pr.Number, 10))
	b.WriteString(`,"state":`)
	b.WriteString(jstr(pr.State))
	b.WriteString(`,"title":`)
	b.WriteString(jstr(pr.Title))
	b.WriteString(`,"body":`)
	b.WriteString(jstr(pr.Body))
	b.WriteString(`,"user":{"login":"adaiversen","id":1000,"type":"User"},"head":{"ref":`)
	b.WriteString(jstr(pr.HeadRef))
	b.WriteString(`,"sha":`)
	b.WriteString(jstr(headSHA))
	b.WriteString(`,"label":`)
	b.WriteString(jstr(r.Owner + ":" + pr.HeadRef))
	b.WriteString(`},"base":{"ref":`)
	b.WriteString(jstr(pr.BaseRef))
	b.WriteString(`,"sha":`)
	b.WriteString(jstr(baseSHA))
	b.WriteString(`,"label":`)
	b.WriteString(jstr(r.Owner + ":" + pr.BaseRef))
	b.WriteString(`},"merged":`)
	if pr.Merged {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"mergeable":`)
	if r.prMergeable(pr) {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"merge_commit_sha":`)
	if pr.MergeSHA == "" {
		b.WriteString("null")
	} else {
		b.WriteString(jstr(pr.MergeSHA))
	}
	b.WriteString(`,"merged_at":`)
	if pr.MergedAt == "" {
		b.WriteString("null")
	} else {
		b.WriteString(jstr(pr.MergedAt))
	}
	b.WriteString(`,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","closed_at":`)
	if pr.State == "closed" {
		b.WriteString(`"2025-01-01T00:00:00Z"`)
	} else {
		b.WriteString("null")
	}
	b.WriteString(`}`)
	return b.String()
}

// commentJSON builds one issue comment object.
func commentJSON(c *Comment) string {
	return `{"id":` + strconv.FormatInt(c.ID, 10) + `,"body":` + jstr(c.Body) +
		`,"user":{"login":"adaiversen","id":1000,"type":"User"},"created_at":"2025-01-01T00:00:00Z"}`
}
