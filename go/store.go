// Meridian Forge — in-memory store.
//
// Everything the forge serves lives in memory: git objects and refs, issues,
// pull requests, labels and comments. One global RWMutex guards the store;
// reads (the hot paths) share the read lock, mutations take the write lock.
//
// Determinism: no wall clock is ever read for state. All timestamps derive
// from a fixed base epoch (MAKEFUN_TRIAL_NOW when set, else the scenario
// epoch) plus a monotonic counter, so the same request sequence produces the
// same bytes. All ids are sequential counters. List order is always explicit
// (creation order or sorted), never map iteration order.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The token owner. The suite's scenario is drawn from a fixed public seed,
// so the viewer login is the same in every trial (wire.go buildScenario).
const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"

// Fallback epoch when MAKEFUN_TRIAL_NOW is unset or unparsable:
// 2025-01-01T00:00:00Z, the scenario's day one.
const fallbackEpochSec = 1735689600

// Git object types (pack type codes).
const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

// Object is one git object: its type and raw bytes.
type Object struct {
	Type byte
	Data []byte
}

// hashObject returns the SHA-1 id of a git object: sha1("<type> <len>\0" + data).
func hashObject(typ byte, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typeName(typ)))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func typeName(typ byte) string {
	switch typ {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "unknown"
}

func typeCode(name string) byte {
	switch name {
	case "commit":
		return objCommit
	case "tree":
		return objTree
	case "blob":
		return objBlob
	case "tag":
		return objTag
	}
	return 0
}

// Label is a repository label.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Issue is one issue. JSON is pre-serialized at creation/mutation and served
// verbatim on the read hot path. nodeID and labelsJSON are immutable after
// creation (labels never change), so the refresh only rebuilds the mutable
// fields around them.
type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []*Label
	CreatedAt  string
	UpdatedAt  string
	CreatedSeq int64
	Comments   []*Comment
	nodeID     string
	labelsJSON []byte
	jsonCache  []byte
}

// Comment is one issue comment.
type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
}

// Pull is one pull request.
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	Merged         bool
	MergedAt       string // "" until merged
	MergeCommitSHA string // "" until merged
	CreatedAt      string
	UpdatedAt      string
	CreatedSeq     int64
}

// Repo is one repository: its git store plus its issues, pulls and labels.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	objects       map[string]*Object
	refs          map[string]string // ref name -> oid
	commitParents map[string][]string
	commitTree    map[string]string
	commitMessage map[string]string

	issues     map[int]*Issue
	issueOrder []int
	labels     map[string]*Label
	labelOrder []string
	pulls      map[int]*Pull
	pullOrder  []int
	nextNumber int // issues and pulls share GitHub's number sequence
	nextComment int

	// gen bumps on every ref or object change; caches are keyed by it.
	gen uint64

	// Response caches, invalidated by gen.
	advV2     []byte
	advV2Gen  uint64
	advUpV0   []byte
	advUpGen  uint64
	advRecvV0 []byte
	advRecvGen uint64
	lsCache   map[string]cachedResp
	fetchCache map[string]cachedResp
	packCache map[string][]byte
}

type cachedResp struct {
	gen  uint64
	body []byte
}

func newRepo(id int, owner, name string, private bool, description, createdAt string) *Repo {
	return &Repo{
		ID:            id,
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		objects:       make(map[string]*Object),
		refs:          make(map[string]string),
		commitParents: make(map[string][]string),
		commitTree:    make(map[string]string),
		commitMessage: make(map[string]string),
		issues:        make(map[int]*Issue),
		labels:        make(map[string]*Label),
		pulls:         make(map[int]*Pull),
		nextNumber:    1,
		nextComment:   1,
		lsCache:       make(map[string]cachedResp),
		fetchCache:    make(map[string]cachedResp),
		packCache:     make(map[string][]byte),
	}
}

func (r *Repo) fullName() string { return r.Owner + "/" + r.Name }

// globalID returns the opaque Relay-style id for the repo.
func (r *Repo) globalID() string {
	return encodeGlobalID("R", strconv.Itoa(r.ID))
}

func encodeGlobalID(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}

func decodeGlobalID(id string) []string {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		// try padded std encoding too
		b, err = base64.StdEncoding.DecodeString(id)
		if err != nil {
			return nil
		}
	}
	return strings.Split(string(b), ":")
}

// sortedRefNames returns ref names in sorted order (HEAD is handled by callers).
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// headOID is the oid HEAD resolves to (the default branch tip), or "".
func (r *Repo) headOID() string {
	return r.refs["refs/heads/"+r.DefaultBranch]
}

// Store is the whole forge state.
type Store struct {
	mu sync.RWMutex

	repos       map[string]*Repo // "owner/name" -> repo
	repoOrder   []string         // creation order keys
	nextRepoID  int
	nextIssueID int // global issue ids (REST `id`)
	nextPullID  int
	nextCommentID int

	clockBase int64 // epoch seconds
	clockSeq  int64 // monotonic seconds added to the base

	reqSeq atomic.Uint64 // lock-free request-id counter (hot path)
}

func NewStore(clockBase int64) *Store {
	return &Store{
		repos:         make(map[string]*Repo),
		nextRepoID:    1,
		nextIssueID:   1,
		nextPullID:    1,
		nextCommentID: 1,
		clockBase:     clockBase,
	}
}

// nextTime returns the next deterministic timestamp (epoch seconds).
func (s *Store) nextTime() int64 {
	s.clockSeq++
	return s.clockBase + s.clockSeq
}

// nextTimeISO returns the next deterministic timestamp as RFC 3339 UTC.
func (s *Store) nextTimeISO() string {
	return formatEpoch(s.nextTime())
}

// formatEpoch formats epoch seconds as RFC 3339 UTC ("2006-01-02T15:04:05Z")
// with integer arithmetic instead of time.Time (the write hot path formats
// one timestamp per issue create). Output is byte-identical to
// time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z") for sec >= 0;
// negative inputs fall back to time.Format.
func formatEpoch(sec int64) string {
	if sec < 0 {
		return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
	}
	days := sec / 86400
	rem := sec % 86400
	hour := rem / 3600
	min := (rem % 3600) / 60
	secOfMin := rem % 60
	// Civil from days (Howard Hinnant's algorithm).
	z := days + 719468
	era := z / 146097
	doe := z - era*146097 // [0, 146096]
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365 // [0, 399]
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100) // [0, 365]
	mp := (5*doy + 2) / 153                   // [0, 11]
	d := doy - (153*mp+2)/5 + 1               // [1, 31]
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
		y++
	}
	var b [20]byte
	b[0] = byte('0' + y/1000)
	b[1] = byte('0' + (y/100)%10)
	b[2] = byte('0' + (y/10)%10)
	b[3] = byte('0' + y%10)
	b[4] = '-'
	b[5] = byte('0' + m/10)
	b[6] = byte('0' + m%10)
	b[7] = '-'
	b[8] = byte('0' + d/10)
	b[9] = byte('0' + d%10)
	b[10] = 'T'
	b[11] = byte('0' + hour/10)
	b[12] = byte('0' + hour%10)
	b[13] = ':'
	b[14] = byte('0' + min/10)
	b[15] = byte('0' + min%10)
	b[16] = ':'
	b[17] = byte('0' + secOfMin/10)
	b[18] = byte('0' + secOfMin%10)
	b[19] = 'Z'
	return string(b[:])
}

// nextRequestID returns a deterministic request id for X-GitHub-Request-Id.
// Lock-free: the hot paths call this on every request.
func (s *Store) nextRequestID() string {
	return "meridian-" + strconv.FormatUint(s.reqSeq.Add(1), 10)
}

// getRepo returns the repo for owner/name, or nil. Caller must hold a lock.
func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// createRepo creates a repository under the viewer. Returns nil if it exists.
func (s *Store) createRepo(name string, private bool, description string) *Repo {
	key := viewerLogin + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil
	}
	now := s.nextTimeISO()
	r := newRepo(s.nextRepoID, viewerLogin, name, private, description, now)
	s.nextRepoID++
	s.repos[key] = r
	s.repoOrder = append(s.repoOrder, key)
	return r
}

// deleteRepo removes a repository. Returns false if absent.
func (s *Store) deleteRepo(owner, name string) bool {
	key := owner + "/" + name
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	for i, k := range s.repoOrder {
		if k == key {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// putObject stores an object, parsing commit metadata. Returns its oid.
// Caller must hold the write lock.
func (r *Repo) putObject(typ byte, data []byte) string {
	oid := hashObject(typ, data)
	r.putObjectWithOID(oid, typ, data)
	return oid
}

// putObjectWithOID stores an object whose oid is already known (readPack
// hashes every object it returns, so the push path must not hash twice).
// Caller must hold the write lock.
func (r *Repo) putObjectWithOID(oid string, typ byte, data []byte) {
	if _, ok := r.objects[oid]; ok {
		return
	}
	r.objects[oid] = &Object{Type: typ, Data: data}
	if typ == objCommit {
		tree, parents, msg := parseCommit(data)
		r.commitTree[oid] = tree
		r.commitParents[oid] = parents
		r.commitMessage[oid] = msg
	}
}

// isAncestor reports whether anc is reachable from oid (following parents),
// peeling tag objects to their target commit first.
func (r *Repo) isAncestor(anc, oid string) bool {
	if anc == oid {
		return true
	}
	anc = r.peelToCommit(anc)
	oid = r.peelToCommit(oid)
	if anc == "" || oid == "" || anc == oid {
		return anc == oid && anc != ""
	}
	seen := map[string]bool{oid: true}
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range r.commitParents[cur] {
			if p == anc {
				return true
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

// peelToCommit follows tag objects to the commit they point at.
func (r *Repo) peelToCommit(oid string) string {
	for i := 0; i < 16; i++ {
		obj := r.objects[oid]
		if obj == nil {
			return ""
		}
		if obj.Type != objTag {
			return oid
		}
		target, _ := parseTagTarget(obj.Data)
		if target == "" {
			return ""
		}
		oid = target
	}
	return ""
}

// mergeBase finds a common ancestor of a and b (first found walking from b
// into a's ancestor set). Returns "" when there is none.
func (r *Repo) mergeBase(a, b string) string {
	a = r.peelToCommit(a)
	b = r.peelToCommit(b)
	if a == "" || b == "" {
		return ""
	}
	ancestors := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		stack = append(stack, r.commitParents[cur]...)
	}
	seen := map[string]bool{}
	stack = []string{b}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		stack = append(stack, r.commitParents[cur]...)
	}
	return ""
}

// reachableObjects returns the set of oids reachable from tips (commits,
// trees, blobs, tags), following commit parents and tree entries.
func (r *Repo) reachableObjects(tips []string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string{}, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj := r.objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = true
		switch obj.Type {
		case objCommit:
			tree, parents, _ := parseCommit(obj.Data)
			if tree != "" {
				stack = append(stack, tree)
			}
			stack = append(stack, parents...)
		case objTree:
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		case objTag:
			target, _ := parseTagTarget(obj.Data)
			if target != "" {
				stack = append(stack, target)
			}
		}
	}
	return out
}

// treeEntriesAt returns the full path -> (oid, mode) map of the tree at
// the given commit (or tree oid), walking recursively.
func (r *Repo) treeEntriesAt(commitOID string) map[string]treeEntry {
	out := map[string]treeEntry{}
	treeOID := r.commitTree[commitOID]
	if treeOID == "" {
		// maybe a tree oid was passed directly
		if obj := r.objects[commitOID]; obj != nil && obj.Type == objTree {
			treeOID = commitOID
		} else {
			return out
		}
	}
	r.walkTree(treeOID, "", out)
	return out
}

type treeEntry struct {
	OID  string
	Mode string
}

func (r *Repo) walkTree(treeOID, prefix string, out map[string]treeEntry) {
	obj := r.objects[treeOID]
	if obj == nil || obj.Type != objTree {
		return
	}
	for _, e := range parseTree(obj.Data) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			r.walkTree(e.OID, path+"/", out)
		} else {
			out[path] = treeEntry{OID: e.OID, Mode: e.Mode}
		}
	}
}

// blobAt returns the blob oid at path in the given commit's tree, or "".
func (r *Repo) blobAt(commitOID, path string) string {
	if e, ok := r.treeEntriesAt(commitOID)[path]; ok {
		return e.OID
	}
	return ""
}

// commitList returns the commits reachable from tip in newest-first order
// (DFS pre-order following parents), deduplicated.
func (r *Repo) commitList(tip string) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		seen[oid] = true
		if r.objects[oid] == nil || r.objects[oid].Type != objCommit {
			return
		}
		out = append(out, oid)
		for _, p := range r.commitParents[oid] {
			visit(p)
		}
	}
	visit(tip)
	return out
}

// errString formats an error for a detail line.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
