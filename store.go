// In-memory store: one process, one store. Repos hold objects, refs, issues,
// pull requests, labels and comments. No wall clock anywhere: every stored
// timestamp is the fixed epoch, so replay exports byte-identical state.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
)

const (
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	viewerID    = 1000
	fixedEpoch  = 1735689600
	fixedISO    = "2025-01-01T00:00:00Z"
	fixedWho    = "ada iversen <adaiversen@example.invalid> 1735689600 +0000"
)

func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func parseGlobalID(s string) (kind, key string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", false
	}
	i := strings.IndexByte(string(b), ':')
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}

type label struct {
	Name  string
	Color string
}

type comment struct {
	ID   int
	Body string
}

type issue struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*comment
	cached   []byte // serialized GET issue body
}

type pullRequest struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	HeadRef  string
	BaseRef  string
	Merged   bool
	MergeSHA string
	MergedAt string
}

type fetchEntry struct {
	gen  int
	body []byte
}

type repo struct {
	Owner       string
	Name        string
	Description string
	Private     bool
	ID          int
	Objects     map[string]*gitObject
	Refs        map[string]string
	RefOrder    []string
	Labels      map[string]*label
	LabelOrder  []string
	Issues      []*issue
	PRs         []*pullRequest
	NextNumber  int
	Gen         int
	FetchCache  map[string]*fetchEntry
	FetchOrder  []string
	AdvCache    map[string]*fetchEntry
	RepoJSON    []byte
}

func (r *repo) getObject(oid string) *gitObject {
	return r.Objects[oid]
}

func (r *repo) putObject(o *gitObject) string {
	oid := objectOID(o)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = o
	}
	return oid
}

func (r *repo) bump() {
	r.Gen++
	r.RepoJSON = nil
}

func (r *repo) setRef(ref, oid string) {
	if _, ok := r.Refs[ref]; !ok {
		r.RefOrder = append(r.RefOrder, ref)
	}
	r.Refs[ref] = oid
	r.bump()
}

func (r *repo) deleteRef(ref string) {
	if _, ok := r.Refs[ref]; !ok {
		return
	}
	delete(r.Refs, ref)
	for i, n := range r.RefOrder {
		if n == ref {
			r.RefOrder = append(r.RefOrder[:i], r.RefOrder[i+1:]...)
			break
		}
	}
	r.bump()
}

func (r *repo) refTip(ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}

func (r *repo) defaultTip() (string, bool) {
	return r.refTip("refs/heads/main")
}

func (r *repo) findIssue(n int) *issue {
	for _, is := range r.Issues {
		if is.Number == n {
			return is
		}
	}
	return nil
}

func (r *repo) findPR(n int) *pullRequest {
	for _, pr := range r.PRs {
		if pr.Number == n {
			return pr
		}
	}
	return nil
}

func (r *repo) findItem(n int) (*issue, *pullRequest) {
	for _, is := range r.Issues {
		if is.Number == n {
			return is, nil
		}
	}
	for _, pr := range r.PRs {
		if pr.Number == n {
			return nil, pr
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- store

type storeT struct {
	mu           sync.RWMutex
	token        string
	repos        map[string]*repo
	repoOrder    []*repo
	nextRepoID   int
	nextObjectID int
}

var store *storeT

func newStore(token string) *storeT {
	return &storeT{
		token:        token,
		repos:        make(map[string]*repo),
		nextRepoID:   5000,
		nextObjectID: 1,
	}
}

func (s *storeT) createRepo(owner, name, desc string, priv bool) (*repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	r := &repo{
		Owner:       owner,
		Name:        name,
		Description: desc,
		Private:     priv,
		ID:          s.nextRepoID,
		Objects:     make(map[string]*gitObject),
		Refs:        make(map[string]string),
		Labels:      make(map[string]*label),
		NextNumber:  1,
		FetchCache:  make(map[string]*fetchEntry),
		AdvCache:    make(map[string]*fetchEntry),
	}
	s.nextRepoID++
	s.repos[key] = r
	s.repoOrder = append(s.repoOrder, r)
	return r, true
}

func (s *storeT) findRepo(owner, name string) *repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[owner+"/"+name]
}

func (s *storeT) deleteRepo(r *repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, r.Owner+"/"+r.Name)
	for i, x := range s.repoOrder {
		if x == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
}

func (s *storeT) nextID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextObjectID
	s.nextObjectID++
	return id
}

// ---------------------------------------------------------------- JSON builders

func (s *storeT) repoJSON(r *repo) []byte {
	if r.RepoJSON != nil {
		return r.RepoJSON
	}
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	sb.WriteString(strconv.Itoa(r.ID))
	sb.WriteString(`,"node_id":`)
	jsonEscape(&sb, globalID("Repository", r.Owner+"/"+r.Name))
	sb.WriteString(`,"name":`)
	jsonEscape(&sb, r.Name)
	sb.WriteString(`,"full_name":`)
	jsonEscape(&sb, r.Owner+"/"+r.Name)
	sb.WriteString(`,"owner":{"login":`)
	jsonEscape(&sb, r.Owner)
	sb.WriteString(`,"id":`)
	sb.WriteString(strconv.Itoa(viewerID))
	sb.WriteString(`,"type":"User"},"private":`)
	sb.WriteString(strconv.FormatBool(r.Private))
	sb.WriteString(`,"description":`)
	if r.Description == "" {
		sb.WriteString("null")
	} else {
		jsonEscape(&sb, r.Description)
	}
	sb.WriteString(`,"default_branch":"main","html_url":`)
	jsonEscape(&sb, "https://github.com/"+r.Owner+"/"+r.Name)
	sb.WriteString(`,"url":`)
	jsonEscape(&sb, "https://api.github.com/repos/"+r.Owner+"/"+r.Name)
	sb.WriteString(`,"created_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"updated_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"has_issues":true,"fork":false}`)
	r.RepoJSON = []byte(sb.String())
	return r.RepoJSON
}

func (s *storeT) issueJSON(r *repo, is *issue) []byte {
	if is.cached != nil {
		return is.cached
	}
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	sb.WriteString(strconv.Itoa(is.ID))
	sb.WriteString(`,"node_id":`)
	jsonEscape(&sb, globalID("Issue", r.Owner+"/"+r.Name+"/"+strconv.Itoa(is.Number)))
	sb.WriteString(`,"number":`)
	sb.WriteString(strconv.Itoa(is.Number))
	sb.WriteString(`,"title":`)
	jsonEscape(&sb, is.Title)
	sb.WriteString(`,"body":`)
	jsonEscape(&sb, is.Body)
	sb.WriteString(`,"state":`)
	jsonEscape(&sb, is.State)
	sb.WriteString(`,"labels":[`)
	for i, ln := range is.Labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"name":`)
		jsonEscape(&sb, ln)
		sb.WriteString(`,"color":`)
		if lb, ok := r.Labels[ln]; ok {
			jsonEscape(&sb, lb.Color)
		} else {
			sb.WriteString(`"ededed"`)
		}
		sb.WriteByte('}')
	}
	sb.WriteString(`],"user":{"login":`)
	jsonEscape(&sb, viewerLogin)
	sb.WriteString(`,"id":`)
	sb.WriteString(strconv.Itoa(viewerID))
	sb.WriteString(`,"type":"User"},"assignee":null,"created_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"updated_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"closed_at":`)
	if is.State == "closed" {
		jsonEscape(&sb, fixedISO)
	} else {
		sb.WriteString("null")
	}
	sb.WriteString(`,"comments":`)
	sb.WriteString(strconv.Itoa(len(is.Comments)))
	sb.WriteByte('}')
	is.cached = []byte(sb.String())
	return is.cached
}

func (s *storeT) prJSON(r *repo, pr *pullRequest) []byte {
	headSHA, _ := r.refTip("refs/heads/" + pr.HeadRef)
	baseSHA, _ := r.refTip("refs/heads/" + pr.BaseRef)
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	sb.WriteString(strconv.Itoa(pr.ID))
	sb.WriteString(`,"node_id":`)
	jsonEscape(&sb, globalID("PullRequest", r.Owner+"/"+r.Name+"/"+strconv.Itoa(pr.Number)))
	sb.WriteString(`,"number":`)
	sb.WriteString(strconv.Itoa(pr.Number))
	sb.WriteString(`,"state":`)
	jsonEscape(&sb, pr.State)
	sb.WriteString(`,"title":`)
	jsonEscape(&sb, pr.Title)
	sb.WriteString(`,"body":`)
	jsonEscape(&sb, pr.Body)
	sb.WriteString(`,"user":{"login":`)
	jsonEscape(&sb, viewerLogin)
	sb.WriteString(`,"id":`)
	sb.WriteString(strconv.Itoa(viewerID))
	sb.WriteString(`,"type":"User"},"head":{"ref":`)
	jsonEscape(&sb, pr.HeadRef)
	sb.WriteString(`,"sha":`)
	jsonEscape(&sb, headSHA)
	sb.WriteString(`,"label":`)
	jsonEscape(&sb, r.Owner+":"+pr.HeadRef)
	sb.WriteString(`},"base":{"ref":`)
	jsonEscape(&sb, pr.BaseRef)
	sb.WriteString(`,"sha":`)
	jsonEscape(&sb, baseSHA)
	sb.WriteString(`,"label":`)
	jsonEscape(&sb, r.Owner+":"+pr.BaseRef)
	sb.WriteString(`},"merged":`)
	sb.WriteString(strconv.FormatBool(pr.Merged))
	sb.WriteString(`,"mergeable":`)
	sb.WriteString(strconv.FormatBool(s.prMergeable(r, pr)))
	sb.WriteString(`,"merge_commit_sha":`)
	if pr.MergeSHA == "" {
		sb.WriteString("null")
	} else {
		jsonEscape(&sb, pr.MergeSHA)
	}
	sb.WriteString(`,"merged_at":`)
	if pr.MergedAt == "" {
		sb.WriteString("null")
	} else {
		jsonEscape(&sb, pr.MergedAt)
	}
	sb.WriteString(`,"created_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"updated_at":`)
	jsonEscape(&sb, fixedISO)
	sb.WriteString(`,"closed_at":`)
	if pr.State == "closed" {
		jsonEscape(&sb, fixedISO)
	} else {
		sb.WriteString("null")
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// prMergeable reports whether a squash merge of head into base would succeed.
func (s *storeT) prMergeable(r *repo, pr *pullRequest) bool {
	if pr.State != "open" {
		return false
	}
	headSHA, ok1 := r.refTip("refs/heads/" + pr.HeadRef)
	baseSHA, ok2 := r.refTip("refs/heads/" + pr.BaseRef)
	if !ok1 || !ok2 {
		return false
	}
	get := r.getObject
	hc := get(headSHA)
	bc := get(baseSHA)
	if hc == nil || bc == nil || hc.typ != objCommit || bc.typ != objCommit {
		return false
	}
	mb := mergeBase(get, headSHA, baseSHA)
	if mb == "" {
		return false
	}
	mbc := get(mb)
	if mbc == nil || mbc.typ != objCommit {
		return false
	}
	_, conflict := threeWayMerge(get, parseCommit(mbc.data).tree, parseCommit(bc.data).tree, parseCommit(hc.data).tree)
	return !conflict
}

// mergePR performs a squash merge. Returns (mergeSHA, conflict, ok).
func (s *storeT) mergePR(r *repo, pr *pullRequest) (string, bool, bool) {
	if pr.State != "open" {
		return "", false, false
	}
	headSHA, ok1 := r.refTip("refs/heads/" + pr.HeadRef)
	baseSHA, ok2 := r.refTip("refs/heads/" + pr.BaseRef)
	if !ok1 || !ok2 {
		return "", false, false
	}
	get := r.getObject
	hc := get(headSHA)
	bc := get(baseSHA)
	if hc == nil || bc == nil || hc.typ != objCommit || bc.typ != objCommit {
		return "", false, false
	}
	mb := mergeBase(get, headSHA, baseSHA)
	if mb == "" {
		return "", false, false
	}
	mbc := get(mb)
	if mbc == nil || mbc.typ != objCommit {
		return "", false, false
	}
	merged, conflict := threeWayMerge(get, parseCommit(mbc.data).tree, parseCommit(bc.data).tree, parseCommit(hc.data).tree)
	if conflict {
		return "", true, true
	}
	treeOID := buildTreeFromFlat(func(oid string, o *gitObject) { r.putObject(o) }, merged)
	msg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + r.Owner + "/" + pr.HeadRef + "\n\n" + pr.Title + "\n"
	data := buildCommit(treeOID, []string{baseSHA}, fixedWho, fixedWho, msg)
	oid := r.putObject(&gitObject{typ: objCommit, data: data})
	r.setRef("refs/heads/"+pr.BaseRef, oid)
	pr.State = "closed"
	pr.Merged = true
	pr.MergeSHA = oid
	pr.MergedAt = fixedISO
	return oid, false, true
}

// ---------------------------------------------------------------- fetch planning

type fetchPlan struct {
	send      []string
	shallow   []string
	unshallow []string
}

// planFetch computes which objects a fetch must deliver.
// wants: tip oids; haves: oids the client already holds;
// clientShallow: the client's current shallow boundary; depth: 0 = full fetch.
// haveClosure computes the objects the client already holds: the closure of
// haves, but never following the parents of commits named in clientShallow.
// Those are shallow-boundary commits — the client holds the commit and its
// tree, but NOT its parents. Using the full closure here would wrongly
// exclude the parent a deepen is supposed to deliver.
func haveClosure(get func(string) *gitObject, haves, clientShallow []string) map[string]bool {
	shallowSet := make(map[string]bool, len(clientShallow))
	for _, s := range clientShallow {
		shallowSet[s] = true
	}
	seen := make(map[string]bool)
	var stack []string
	stack = append(stack, haves...)
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
			if !shallowSet[oid] {
				stack = append(stack, c.parents...)
			}
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

func planFetch(r *repo, wants, haves, clientShallow []string, depth int) fetchPlan {
	get := r.getObject
	var plan fetchPlan
	if depth == 0 {
		haveReach := haveClosure(get, haves, clientShallow)
		wantReach := collectReachable(get, wants)
		for oid := range wantReach {
			if !haveReach[oid] {
				plan.send = append(plan.send, oid)
			}
		}
		// A full fetch unshallows the client's old boundary commits: their
		// full history is delivered now.
		plan.unshallow = append(plan.unshallow, clientShallow...)
		return plan
	}
	// Depth-limited: include commits within <depth generations of wants.
	dist := make(map[string]int)
	queue := make([]string, 0, len(wants))
	for _, w := range wants {
		if _, ok := dist[w]; !ok {
			dist[w] = 0
			queue = append(queue, w)
		}
	}
	included := make(map[string]bool) // commit oids included
	frontier := make(map[string]bool) // included commits at distance depth-1
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		d := dist[oid]
		if d >= depth {
			continue
		}
		o := get(oid)
		if o == nil {
			continue
		}
		if o.typ == objTag {
			target, _, _, _, _ := parseTag(o.data)
			if _, ok := dist[target]; !ok {
				dist[target] = d
				queue = append(queue, target)
			}
			continue
		}
		if o.typ != objCommit {
			continue
		}
		included[oid] = true
		if d == depth-1 {
			frontier[oid] = true
		}
		c := parseCommit(o.data)
		for _, p := range c.parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				queue = append(queue, p)
			}
		}
	}
	// Tree closures of included commits.
	sendSet := make(map[string]bool)
	for oid := range included {
		sendSet[oid] = true
		c := parseCommit(get(oid).data)
		for t := range collectReachable(get, []string{c.tree}) {
			sendSet[t] = true
		}
	}
	// Tag roots themselves must be sent too (a want may name a tag object).
	for _, w := range wants {
		if o := get(w); o != nil && o.typ == objTag {
			sendSet[w] = true
		}
	}
	haveReach := haveClosure(get, haves, clientShallow)
	for oid := range sendSet {
		if !haveReach[oid] {
			plan.send = append(plan.send, oid)
		}
	}
	for oid := range frontier {
		plan.shallow = append(plan.shallow, oid)
	}
	for _, oid := range clientShallow {
		if included[oid] && !frontier[oid] {
			plan.unshallow = append(plan.unshallow, oid)
		}
	}
	return plan
}
