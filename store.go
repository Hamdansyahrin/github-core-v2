// The forge's data model: one in-memory store holding repositories, and inside each repository a
// real git object store (blobs, trees, commits, tags keyed by their SHA-1) plus the forge-level
// records (labels, issues, pull requests, comments) that the REST, GraphQL, MCP and git surfaces
// all share. Everything the surfaces serve is derived from these records, so an object created
// through one surface reads back identically through the others.
//
// Determinism: server-created commits take their timestamps from a logical clock seeded at the
// scenario epoch (2025-01-01), never from the wall clock, so two runs of the same request sequence
// produce byte-identical objects.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObj struct {
	Type ObjType
	Data []byte
}

// The token owner's identity: the one user this forge serves (the trial scenario's viewer).
const (
	ViewerLogin = "adaiversen"
	ViewerID    = 1000
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
)

// The all-zero object id: an unborn ref's value in the git protocol.
const ZeroOID = "0000000000000000000000000000000000000000"

// 2025-01-01T00:00:00Z: the logical clock's first tick.
const clockStart = 1735689600

func isoOf(tsSeconds int64) string {
	return time.Unix(tsSeconds, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// git's object id: SHA-1 over `<type> <size>\0` followed by the object's bytes.
func hashObject(objType ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", objType, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------------------------
// trees and commits

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// Parse a tree object's bytes into its entries, in stored order.
func parseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			panic("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			panic("tree entry is truncated")
		}
		nul += space + 1
		if nul+21 > len(data) {
			panic("tree entry is truncated")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

// git's tree sort: byte order of the name, a directory compared as if it ended in '/'.
func compareTreeEntries(a, b TreeEntry) bool {
	left := a.Name
	if a.Mode == "40000" {
		left += "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right += "/"
	}
	return left < right
}

// Serialise tree entries into a tree object's bytes (entries are sorted the way git sorts them).
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeEntries(sorted[i], sorted[j]) })
	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(entry.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
	// Unix seconds from the committer line, or 0 when it does not parse.
	Timestamp int64
}

// Parse a commit object's bytes.
func parseCommit(data []byte) CommitView {
	text := string(data)
	header := text
	message := ""
	if blank := strings.Index(text, "\n\n"); blank >= 0 {
		header = text[:blank]
		message = text[blank+2:]
	}
	view := CommitView{Message: message, Parents: []string{}}
	for _, line := range strings.Split(header, "\n") {
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		key, value := line[:space], line[space+1:]
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	view.Timestamp = commitTimestamp(view.Committer)
	return view
}

// The timestamp in a `Name <email> ts +0000` line: the number before the trailing zone.
func commitTimestamp(line string) int64 {
	fields := strings.Split(line, " ")
	if len(fields) < 2 {
		return 0
	}
	zone := fields[len(fields)-1]
	if len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') {
		return 0
	}
	for _, ch := range zone[1:] {
		if ch < '0' || ch > '9' {
			return 0
		}
	}
	stamp, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0
	}
	return stamp
}

// One `Name <email> timestamp +0000` identity line.
func identityLine(tsSeconds int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", ViewerName, ViewerEmail, tsSeconds)
}

// Serialise a commit object.
func buildCommit(tree string, parents []string, message, author, committer string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, parent := range parents {
		buf.WriteString("parent " + parent + "\n")
	}
	buf.WriteString("author " + author + "\n")
	buf.WriteString("committer " + committer + "\n")
	buf.WriteString("\n")
	if message != "" && !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	buf.WriteString(message)
	return buf.Bytes()
}

// The commit a tag object points at, following tag chains; "" when the chain ends elsewhere.
func parseTagTarget(data []byte) string {
	limit := len(data)
	if limit > 128 {
		limit = 128
	}
	for _, line := range strings.Split(string(data[:limit]), "\n") {
		if strings.HasPrefix(line, "object ") {
			candidate := line[len("object "):]
			if len(candidate) == 40 && isLowerHex(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func isLowerHex(s string) bool {
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------------------------
// records

type LabelRec struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type CommentRec struct {
	ID        int
	Body      string
	CreatedTs int64
}

type IssueRec struct {
	Number     int
	ID         int
	Title      string
	Body       string
	State      string // 'open' | 'closed'
	LabelNames []string
	Comments   []*CommentRec
	CreatedTs  int64
	UpdatedTs  int64
	// Memoised issueJson bytes for the single-issue GET, valid while the fingerprint
	// (UpdatedTs, len(Comments)) matches: every mutation of a rendered field either
	// bumps UpdatedTs (REST patch, GraphQL close) or appends a comment.
	CacheBytes    []byte
	CacheTs       int64
	CacheComments int
}

type PullRec struct {
	Number         int
	ID             int
	Title          string
	Body           string
	State          string // 'open' | 'closed'
	Merged         bool
	MergedAtTs     int64
	HasMergedAt    bool
	MergeCommitSha string
	HasMergeSha    bool
	HeadRef        string
	BaseRef        string
	// The branch tips when the pull request was opened (the fallback when a branch is deleted).
	HeadShaAtCreate string
	BaseShaAtCreate string
	CreatedTs       int64
	UpdatedTs       int64
	MergeableKey    string
	MergeableValue  string
}

type RepoRec struct {
	ID            int
	Owner         string
	Name          string
	IsPrivate     bool
	Description   string
	DefaultBranch string
	CreatedTs     int64
	UpdatedTs     int64
	// The git object store: object id -> object.
	Objects map[string]*GitObj
	// Full ref names ('refs/heads/main') -> object id.
	Refs   map[string]string
	Labels map[string]*LabelRec
	Issues map[int]*IssueRec
	Pulls  map[int]*PullRec
	// One numbering sequence shared by issues and pull requests, as on GitHub.
	NextNumber int
}

// ---------------------------------------------------------------------------------------------
// per-repository object store operations

// Store an object (deduplicated) and answer its id.
func objPut(repo *RepoRec, objType ObjType, data []byte) string {
	oid := hashObject(objType, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObj{Type: objType, Data: data}
	}
	return oid
}

func objGet(repo *RepoRec, oid string) *GitObj {
	return repo.Objects[oid]
}

// The commit at the end of a tag chain, or "" when `oid` does not resolve to a commit.
func resolveCommit(repo *RepoRec, oid string) string {
	current := oid
	for hops := 0; hops < 16; hops++ {
		object := repo.Objects[current]
		if object == nil {
			return ""
		}
		if object.Type == ObjCommit {
			return current
		}
		if object.Type != ObjTag {
			return ""
		}
		target := parseTagTarget(object.Data)
		if target == "" {
			return ""
		}
		current = target
	}
	return ""
}

// The root tree of a commit.
func commitTree(repo *RepoRec, commitOid string) string {
	object := repo.Objects[commitOid]
	if object == nil || object.Type != ObjCommit {
		return ""
	}
	return parseCommit(object.Data).Tree
}

type FlatFile struct {
	Oid  string
	Mode string
}

// Every file under a tree, as a map of slash path -> {oid, mode}. Directories are not entries.
func flattenTree(repo *RepoRec, treeOid string) map[string]FlatFile {
	out := map[string]FlatFile{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		object := repo.Objects[oid]
		if object == nil || object.Type != ObjTree {
			return
		}
		for _, entry := range parseTree(object.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Oid, path)
			} else {
				out[path] = FlatFile{Oid: entry.Oid, Mode: entry.Mode}
			}
		}
	}
	walk(treeOid, "")
	return out
}

// Build (and store) the tree hierarchy holding exactly `files`; answers the root tree id.
func treeFromFlat(repo *RepoRec, files map[string]FlatFile) string {
	type node struct {
		files map[string]FlatFile
		dirs  map[string]*node
	}
	makeNode := func() *node {
		return &node{files: map[string]FlatFile{}, dirs: map[string]*node{}}
	}
	root := makeNode()
	for path, file := range files {
		segments := strings.Split(path, "/")
		current := root
		for index := 0; index < len(segments)-1; index++ {
			name := segments[index]
			if _, ok := current.dirs[name]; !ok {
				current.dirs[name] = makeNode()
			}
			current = current.dirs[name]
		}
		current.files[segments[len(segments)-1]] = file
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := []TreeEntry{}
		for name, file := range n.files {
			entries = append(entries, TreeEntry{Mode: file.Mode, Name: name, Oid: file.Oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: build(child)})
		}
		return objPut(repo, ObjTree, buildTree(entries))
	}
	return build(root)
}

type PathHit struct {
	Kind string // 'file' | 'dir'
	Oid  string
	Mode string
}

// What sits at `path` in a tree: a blob, a sub-tree, or nothing.
func lookupPath(repo *RepoRec, treeOid, path string) *PathHit {
	if path == "" || path == "/" {
		return &PathHit{Kind: "dir", Oid: treeOid, Mode: "40000"}
	}
	segments := strings.Split(path, "/")
	current := treeOid
	for index, segment := range segments {
		object := repo.Objects[current]
		if object == nil || object.Type != ObjTree {
			return nil
		}
		var entry *TreeEntry
		for _, candidate := range parseTree(object.Data) {
			if candidate.Name == segment {
				c := candidate
				entry = &c
				break
			}
		}
		if entry == nil {
			return nil
		}
		if index == len(segments)-1 {
			if entry.Mode == "40000" {
				return &PathHit{Kind: "dir", Oid: entry.Oid, Mode: entry.Mode}
			}
			return &PathHit{Kind: "file", Oid: entry.Oid, Mode: entry.Mode}
		}
		if entry.Mode != "40000" {
			return nil
		}
		current = entry.Oid
	}
	return nil
}

// The entries of the directory at `path` in a tree (for the contents API's directory listing).
func listDir(repo *RepoRec, treeOid, path string) []TreeEntry {
	found := lookupPath(repo, treeOid, path)
	if found == nil || found.Kind != "dir" {
		return nil
	}
	object := repo.Objects[found.Oid]
	if object == nil || object.Type != ObjTree {
		return nil
	}
	return parseTree(object.Data)
}

// ---------------------------------------------------------------------------------------------
// history walks

// Every commit reachable from `tip` (inclusive), following parents.
func ancestorsOf(repo *RepoRec, tip string) map[string]bool {
	seen := map[string]bool{}
	start := resolveCommit(repo, tip)
	if start == "" {
		return seen
	}
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := repo.Objects[oid]
		if object == nil || object.Type != ObjCommit {
			continue
		}
		stack = append(stack, parseCommit(object.Data).Parents...)
	}
	return seen
}

// True when `ancestor` is `descendant` itself or one of its ancestors.
func isAncestor(repo *RepoRec, ancestor, descendant string) bool {
	target := resolveCommit(repo, ancestor)
	start := resolveCommit(repo, descendant)
	if target == "" || start == "" {
		return false
	}
	if target == start {
		return true
	}
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == target {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := repo.Objects[oid]
		if object == nil || object.Type != ObjCommit {
			continue
		}
		stack = append(stack, parseCommit(object.Data).Parents...)
	}
	return false
}

// The history from `tip`, newest first (committer timestamp, then discovery order).
func logFrom(repo *RepoRec, tip string) []string {
	start := resolveCommit(repo, tip)
	if start == "" {
		return []string{}
	}
	order := []string{}
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
		object := repo.Objects[oid]
		if object == nil || object.Type != ObjCommit {
			continue
		}
		parents := parseCommit(object.Data).Parents
		for index := len(parents) - 1; index >= 0; index-- {
			stack = append(stack, parents[index])
		}
	}
	type stamped struct {
		oid   string
		index int
		stamp int64
	}
	entries := make([]stamped, len(order))
	for index, oid := range order {
		var stamp int64
		if object := repo.Objects[oid]; object != nil && object.Type == ObjCommit {
			stamp = parseCommit(object.Data).Timestamp
		}
		entries[index] = stamped{oid: oid, index: index, stamp: stamp}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].stamp != entries[j].stamp {
			return entries[i].stamp > entries[j].stamp
		}
		return entries[i].index < entries[j].index
	})
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.oid
	}
	return out
}

// The best common ancestor of two commits, or "" when they share no history.
func mergeBaseOf(repo *RepoRec, a, b string) string {
	startA := resolveCommit(repo, a)
	startB := resolveCommit(repo, b)
	if startA == "" || startB == "" {
		return ""
	}
	ancestors := ancestorsOf(repo, startA)
	if ancestors[startB] {
		return startB
	}
	seen := map[string]bool{startB: true}
	queue := []string{startB}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancestors[oid] {
			return oid
		}
		object := repo.Objects[oid]
		if object == nil || object.Type != ObjCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// merging and diffing

// Three-way merge of two trees against their base. Answers the merged tree id, or "" when a
// path changed differently on both sides (a conflict).
func mergeTrees(repo *RepoRec, baseTree, ourTree, theirTree string) string {
	base := flattenTree(repo, baseTree)
	ours := flattenTree(repo, ourTree)
	theirs := flattenTree(repo, theirTree)
	merged := map[string]FlatFile{}
	paths := map[string]bool{}
	for path := range base {
		paths[path] = true
	}
	for path := range ours {
		paths[path] = true
	}
	for path := range theirs {
		paths[path] = true
	}
	same := func(x, y *FlatFile) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Oid == y.Oid
	}
	get := func(m map[string]FlatFile, path string) *FlatFile {
		if file, ok := m[path]; ok {
			return &file
		}
		return nil
	}
	for path := range paths {
		b := get(base, path)
		o := get(ours, path)
		t := get(theirs, path)
		switch {
		case same(o, t):
			if o != nil {
				merged[path] = *o
			}
		case same(b, o):
			if t != nil {
				merged[path] = *t
			}
		case same(b, t):
			if o != nil {
				merged[path] = *o
			}
		default:
			return ""
		}
	}
	return treeFromFlat(repo, merged)
}

type FileChange struct {
	Filename  string
	Status    string // 'added' | 'removed' | 'modified'
	Additions int
	Deletions int
	Changes   int
}

// The lines of a blob, for diff counting: a trailing newline does not start a new line.
func linesOf(data []byte) []string {
	if len(data) == 0 {
		return []string{}
	}
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Line-level diff counts between two blobs (longest-common-subsequence based).
func diffCounts(repo *RepoRec, oldOid, newOid string) (additions, deletions int) {
	var oldLines, newLines []string
	if oldOid != "" {
		if object := repo.Objects[oldOid]; object != nil {
			oldLines = linesOf(object.Data)
		}
	}
	if newOid != "" {
		if object := repo.Objects[newOid]; object != nil {
			newLines = linesOf(object.Data)
		}
	}
	if oldLines == nil {
		oldLines = []string{}
	}
	if newLines == nil {
		newLines = []string{}
	}
	if oldOid == "" {
		return len(newLines), 0
	}
	if newOid == "" {
		return 0, len(oldLines)
	}
	if len(oldLines)*len(newLines) > 4_000_000 {
		return len(newLines), len(oldLines)
	}
	width := len(newLines) + 1
	previous := make([]uint32, width)
	current := make([]uint32, width)
	for i := 1; i <= len(oldLines); i++ {
		for j := 1; j <= len(newLines); j++ {
			if oldLines[i-1] == newLines[j-1] {
				current[j] = previous[j-1] + 1
			} else if previous[j] > current[j-1] {
				current[j] = previous[j]
			} else {
				current[j] = current[j-1]
			}
		}
		previous, current = current, previous
		for j := range current {
			current[j] = 0
		}
	}
	common := int(previous[len(newLines)])
	return len(newLines) - common, len(oldLines) - common
}

// Every path that differs between two commits' trees, with line counts.
func diffFiles(repo *RepoRec, baseCommit, headCommit string) []FileChange {
	baseTree := commitTree(repo, baseCommit)
	headTree := commitTree(repo, headCommit)
	if baseTree == "" || headTree == "" {
		return []FileChange{}
	}
	base := flattenTree(repo, baseTree)
	head := flattenTree(repo, headTree)
	pathSet := map[string]bool{}
	for path := range base {
		pathSet[path] = true
	}
	for path := range head {
		pathSet[path] = true
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	changes := []FileChange{}
	for _, path := range paths {
		b, hasB := base[path]
		h, hasH := head[path]
		if hasB && hasH && b.Oid == h.Oid {
			continue
		}
		oldOid, newOid := "", ""
		if hasB {
			oldOid = b.Oid
		}
		if hasH {
			newOid = h.Oid
		}
		adds, dels := diffCounts(repo, oldOid, newOid)
		status := "modified"
		if !hasB {
			status = "added"
		} else if !hasH {
			status = "removed"
		}
		changes = append(changes, FileChange{
			Filename:  path,
			Status:    status,
			Additions: adds,
			Deletions: dels,
			Changes:   adds + dels,
		})
	}
	return changes
}

// ---------------------------------------------------------------------------------------------
// global ids (GraphQL node ids: opaque, stable, never the plain database id)

// Encode a global id: base64 of `kind:part:part...` (R = repository, I = issue, P = pull, C = comment, U = user).
func gidEncode(kind string, parts ...any) string {
	strs := make([]string, 0, len(parts)+1)
	strs = append(strs, kind)
	for _, part := range parts {
		switch value := part.(type) {
		case string:
			strs = append(strs, value)
		case int:
			strs = append(strs, strconv.Itoa(value))
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(strs, ":")))
}

// Decode a global id into its parts, or nil when it is not one of ours.
func gidDecode(id string) []string {
	decoded, ok := base64LenientDecode(id)
	if !ok {
		return nil
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) < 2 || len(parts[0]) != 1 || !strings.ContainsRune("RIPCU", rune(parts[0][0])) {
		return nil
	}
	return parts
}

// Base64 decoding the way Node's Buffer.from(text, 'base64') behaves: characters outside the
// alphabet are ignored and missing padding is tolerated.
func base64LenientDecode(text string) ([]byte, bool) {
	var cleaned []byte
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '+' || ch == '/' || ch == '=' {
			cleaned = append(cleaned, ch)
		}
	}
	s := string(cleaned)
	if rem := len(s) % 4; rem == 1 {
		s = s[:len(s)-1]
	} else if rem > 0 {
		s += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// ---------------------------------------------------------------------------------------------
// the forge

type Forge struct {
	// Repositories by `<owner>/<name>`, both lowercased (GitHub names are case-insensitive).
	Repos         map[string]*RepoRec
	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	nextLabelID   int
	clock         int64
}

func NewForge() *Forge {
	return &Forge{
		Repos:         map[string]*RepoRec{},
		nextRepoID:    1,
		nextIssueID:   1,
		nextCommentID: 1,
		nextLabelID:   1,
		clock:         clockStart,
	}
}

// The next logical timestamp (seconds): monotonic, deterministic, never the wall clock.
func (f *Forge) NextTs() int64 {
	f.clock++
	return f.clock
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (f *Forge) CreateRepo(owner, name string, isPrivate bool, description string) *RepoRec {
	ts := f.NextTs()
	repo := &RepoRec{
		ID:            f.nextRepoID,
		Owner:         owner,
		Name:          name,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: "main",
		CreatedTs:     ts,
		UpdatedTs:     ts,
		Objects:       map[string]*GitObj{},
		Refs:          map[string]string{},
		Labels:        map[string]*LabelRec{},
		Issues:        map[int]*IssueRec{},
		Pulls:         map[int]*PullRec{},
		NextNumber:    1,
	}
	f.nextRepoID++
	f.Repos[repoKey(owner, name)] = repo
	return repo
}

// A repository by owner and name, or nil. Only the viewer's own repositories exist.
func (f *Forge) GetRepo(owner, name string) *RepoRec {
	if strings.ToLower(owner) != ViewerLogin {
		return nil
	}
	return f.Repos[repoKey(owner, name)]
}

// A repository by its numeric database id (for global-id lookups).
func (f *Forge) RepoById(id int) *RepoRec {
	for _, repo := range f.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (f *Forge) DeleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	if _, ok := f.Repos[key]; !ok {
		return false
	}
	delete(f.Repos, key)
	return true
}

// Every repository the viewer owns, in creation order.
func (f *Forge) ListRepos() []*RepoRec {
	repos := make([]*RepoRec, 0, len(f.Repos))
	for _, repo := range f.Repos {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	return repos
}

func (f *Forge) NewIssueID() int {
	id := f.nextIssueID
	f.nextIssueID++
	return id
}

func (f *Forge) NewCommentID() int {
	id := f.nextCommentID
	f.nextCommentID++
	return id
}

func (f *Forge) NewLabelID() int {
	id := f.nextLabelID
	f.nextLabelID++
	return id
}

// The tip of a branch (full ref or short branch name), or "" when it does not exist.
func (f *Forge) BranchTip(repo *RepoRec, branch string) string {
	ref := branch
	if !strings.HasPrefix(branch, "refs/") {
		ref = "refs/heads/" + branch
	}
	if tip, ok := repo.Refs[ref]; ok {
		return tip
	}
	return ""
}

// Merge a pull request into its base branch (the suite's squash path; the tree merge is the
// same three-way computation whatever the method label). Answers the merge commit id, or
// 'conflict' / 'not-mergeable'.
func (f *Forge) MergePull(repo *RepoRec, pull *PullRec) (string, string) {
	if pull.State != "open" {
		return "", "not-mergeable"
	}
	baseTip := f.BranchTip(repo, pull.BaseRef)
	headTip := f.BranchTip(repo, pull.HeadRef)
	if baseTip == "" || headTip == "" {
		return "", "not-mergeable"
	}
	base := mergeBaseOf(repo, baseTip, headTip)
	if base == "" || base == headTip {
		return "", "not-mergeable"
	}
	var mergedTree string
	if base == baseTip {
		mergedTree = commitTree(repo, headTip)
	} else {
		baseTree := commitTree(repo, base)
		ourTree := commitTree(repo, baseTip)
		theirTree := commitTree(repo, headTip)
		if baseTree == "" || ourTree == "" || theirTree == "" {
			mergedTree = ""
		} else {
			mergedTree = mergeTrees(repo, baseTree, ourTree, theirTree)
		}
	}
	if mergedTree == "" {
		return "", "conflict"
	}
	ts := f.NextTs()
	identity := identityLine(ts)
	commitOid := objPut(repo, ObjCommit, buildCommit(mergedTree, []string{baseTip},
		fmt.Sprintf("%s (#%d)", pull.Title, pull.Number), identity, identity))
	repo.Refs["refs/heads/"+pull.BaseRef] = commitOid
	repo.UpdatedTs = ts
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAtTs = ts
	pull.HasMergedAt = true
	pull.MergeCommitSha = commitOid
	pull.HasMergeSha = true
	pull.UpdatedTs = ts
	return commitOid, ""
}

// GraphQL's mergeable verdict for a pull request, computed from the current branch tips.
func (f *Forge) MergeableState(repo *RepoRec, pull *PullRec) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	baseTip := f.BranchTip(repo, pull.BaseRef)
	headTip := f.BranchTip(repo, pull.HeadRef)
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	key := baseTip + ":" + headTip
	if pull.MergeableKey == key {
		return pull.MergeableValue
	}
	value := "UNKNOWN"
	base := mergeBaseOf(repo, baseTip, headTip)
	if base == baseTip {
		value = "MERGEABLE"
	} else if base != "" {
		baseTree := commitTree(repo, base)
		ourTree := commitTree(repo, baseTip)
		theirTree := commitTree(repo, headTip)
		if baseTree != "" && ourTree != "" && theirTree != "" {
			if mergeTrees(repo, baseTree, ourTree, theirTree) == "" {
				value = "CONFLICTING"
			} else {
				value = "MERGEABLE"
			}
		}
	}
	pull.MergeableKey = key
	pull.MergeableValue = value
	return value
}

// ---------------------------------------------------------------------------------------------
// JSON views (the REST v3 shapes; GraphQL and MCP build theirs from the same records)

// A JSON object under construction (Go maps marshal with sorted keys; the suite parses JSON,
// so member order carries no meaning anywhere except where a surface builds a fixed document).
type JObj = map[string]any

func userJson() JObj {
	return JObj{
		"login": ViewerLogin,
		"id":    ViewerID,
		"type":  "User",
		"name":  ViewerName,
		"email": ViewerEmail,
	}
}

func repoJson(repo *RepoRec) JObj {
	openIssues := 0
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			openIssues++
		}
	}
	return JObj{
		"id":                repo.ID,
		"node_id":           gidEncode("R", repo.ID),
		"name":              repo.Name,
		"full_name":         repo.Owner + "/" + repo.Name,
		"owner":             JObj{"login": repo.Owner, "id": ViewerID, "type": "User"},
		"private":           repo.IsPrivate,
		"description":       repo.Description,
		"default_branch":    repo.DefaultBranch,
		"created_at":        isoOf(repo.CreatedTs),
		"updated_at":        isoOf(repo.UpdatedTs),
		"html_url":          "/" + repo.Owner + "/" + repo.Name,
		"size":              0,
		"stargazers_count":  0,
		"forks_count":       0,
		"open_issues_count": openIssues,
	}
}

func labelJson(label *LabelRec) JObj {
	return JObj{
		"id":          label.ID,
		"name":        label.Name,
		"color":       label.Color,
		"description": label.Description,
		"default":     false,
	}
}

func issueJson(repo *RepoRec, issue *IssueRec) JObj {
	labels := []any{}
	for _, name := range issue.LabelNames {
		if label, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJson(label))
		}
	}
	return JObj{
		"id":         issue.ID,
		"node_id":    gidEncode("I", repo.ID, issue.Number),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       JObj{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"comments":   len(issue.Comments),
		"created_at": isoOf(issue.CreatedTs),
		"updated_at": isoOf(issue.UpdatedTs),
	}
}

func commentJson(comment *CommentRec) JObj {
	return JObj{
		"id":         comment.ID,
		"body":       comment.Body,
		"user":       JObj{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"created_at": isoOf(comment.CreatedTs),
		"updated_at": isoOf(comment.CreatedTs),
	}
}

func pullJson(forge *Forge, repo *RepoRec, pull *PullRec) JObj {
	headSha := forge.BranchTip(repo, pull.HeadRef)
	if headSha == "" {
		headSha = pull.HeadShaAtCreate
	}
	baseSha := forge.BranchTip(repo, pull.BaseRef)
	if baseSha == "" {
		baseSha = pull.BaseShaAtCreate
	}
	var mergedAt any
	if pull.HasMergedAt {
		mergedAt = isoOf(pull.MergedAtTs)
	}
	var mergeSha any
	if pull.HasMergeSha {
		mergeSha = pull.MergeCommitSha
	}
	return JObj{
		"id":               pull.ID,
		"node_id":          gidEncode("P", repo.ID, pull.Number),
		"number":           pull.Number,
		"state":            pull.State,
		"title":            pull.Title,
		"body":             pull.Body,
		"head":             JObj{"ref": pull.HeadRef, "sha": headSha, "label": repo.Owner + ":" + pull.HeadRef},
		"base":             JObj{"ref": pull.BaseRef, "sha": baseSha, "label": repo.Owner + ":" + pull.BaseRef},
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"user":             JObj{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"created_at":       isoOf(pull.CreatedTs),
		"updated_at":       isoOf(pull.UpdatedTs),
	}
}

// ---------------------------------------------------------------------------------------------
// shared creation operations (REST, GraphQL and MCP all create through these)

type CreateIssueResult struct {
	OK      bool
	Issue   *IssueRec
	Status  int
	Message string
}

// Create an issue, validating that every named label exists (GitHub answers 422 otherwise).
func createIssue(forge *Forge, repo *RepoRec, title, body *string, labels []string) CreateIssueResult {
	if title == nil || *title == "" {
		return CreateIssueResult{Status: 422, Message: "Validation Failed: title is missing"}
	}
	labelNames := append([]string{}, labels...)
	for _, name := range labelNames {
		if _, ok := repo.Labels[name]; !ok {
			return CreateIssueResult{Status: 422, Message: fmt.Sprintf("Validation Failed: label '%s' does not exist", name)}
		}
	}
	ts := forge.NextTs()
	issue := &IssueRec{
		Number:     repo.NextNumber,
		ID:         forge.NewIssueID(),
		Title:      *title,
		Body:       strOr(body, ""),
		State:      "open",
		LabelNames: labelNames,
		Comments:   []*CommentRec{},
		CreatedTs:  ts,
		UpdatedTs:  ts,
	}
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.UpdatedTs = ts
	return CreateIssueResult{OK: true, Issue: issue}
}

func strOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

type CreatePullResult struct {
	OK      bool
	Pull    *PullRec
	Status  int
	Message string
}

// A head/base argument as a plain branch name: GitHub's `owner:branch` form loses its prefix.
func branchNameOf(ref string) string {
	if colon := strings.IndexByte(ref, ':'); colon >= 0 {
		return ref[colon+1:]
	}
	return ref
}

// Open a pull request between two existing branches.
func createPull(forge *Forge, repo *RepoRec, title, body, headArg, baseArg *string) CreatePullResult {
	head := ""
	if headArg != nil {
		head = branchNameOf(*headArg)
	}
	base := repo.DefaultBranch
	if baseArg != nil {
		base = branchNameOf(*baseArg)
	}
	headTip := forge.BranchTip(repo, head)
	if head == "" || headTip == "" {
		return CreatePullResult{Status: 422, Message: fmt.Sprintf("Validation Failed: head branch '%s' does not exist", head)}
	}
	baseTip := forge.BranchTip(repo, base)
	if baseTip == "" {
		return CreatePullResult{Status: 422, Message: fmt.Sprintf("Validation Failed: base branch '%s' does not exist", base)}
	}
	if headTip == baseTip {
		return CreatePullResult{Status: 422, Message: "Validation Failed: no commits between the base and head branches"}
	}
	for _, pull := range repo.Pulls {
		if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
			return CreatePullResult{Status: 422, Message: fmt.Sprintf("Validation Failed: a pull request already exists for %s into %s", head, base)}
		}
	}
	ts := forge.NextTs()
	pull := &PullRec{
		Number:          repo.NextNumber,
		ID:              forge.NewIssueID(),
		Title:           strOr(title, ""),
		Body:            strOr(body, ""),
		State:           "open",
		HeadRef:         head,
		BaseRef:         base,
		HeadShaAtCreate: headTip,
		BaseShaAtCreate: baseTip,
		CreatedTs:       ts,
		UpdatedTs:       ts,
		MergeableKey:    "",
		MergeableValue:  "UNKNOWN",
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	repo.UpdatedTs = ts
	return CreatePullResult{OK: true, Pull: pull}
}
