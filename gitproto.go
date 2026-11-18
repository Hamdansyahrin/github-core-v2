// Git smart HTTP: the /<owner>/<repo>.git surface. Protocol v2 for upload-pack (capability
// advertisement, ls-refs, fetch with shallow support), protocol v0 for receive-pack (ref updates
// with report-status in sideband frames) and as the upload-pack fallback. Packfiles are read and
// written here over compress/zlib: pushes arrive as packs (delta-compressed entries resolved
// here), fetches leave as packs of whole objects.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type GitRequest struct {
	Owner string
	Repo  string
	// The URL tail: 'info/refs', 'git-upload-pack' or 'git-receive-pack'.
	Service     string
	Method      string
	Query       url.Values
	GitProtocol string
	Raw         []byte
}

type GitResult struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
}

// ---------------------------------------------------------------------------------------------
// pkt-line

var pktFlush = []byte("0000")

const pktMaxPayload = 65516

// Encode one data packet: 4 hex length digits (payload + 4) then the payload.
func pkt(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out
}

func pktText(text string) []byte {
	return pkt([]byte(text))
}

type packet struct {
	kind    string // 'data' | 'flush' | 'delim'
	payload []byte
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return false
		}
	}
	return true
}

// Parse pkt-lines from `start` until the buffer ends; errors on a malformed frame.
func parsePackets(buffer []byte, start int) ([]packet, int, error) {
	packets := []packet{}
	offset := start
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return nil, 0, errors.New("truncated pkt-line header")
		}
		header := string(buffer[offset : offset+4])
		if !isHex4(header) {
			return nil, 0, fmt.Errorf("invalid pkt-line length %q", header)
		}
		length, _ := strconv.ParseInt(header, 16, 32)
		switch {
		case length == 0:
			packets = append(packets, packet{kind: "flush"})
			offset += 4
		case length == 1:
			packets = append(packets, packet{kind: "delim"})
			offset += 4
		case length == 2:
			// response-end: treat as a flush for parsing purposes
			packets = append(packets, packet{kind: "flush"})
			offset += 4
		case length == 3 || length > 65520:
			return nil, 0, fmt.Errorf("invalid pkt-line length %d", length)
		default:
			if offset+int(length) > len(buffer) {
				return nil, 0, errors.New("truncated pkt-line")
			}
			packets = append(packets, packet{kind: "data", payload: buffer[offset+4 : offset+int(length)]})
			offset += int(length)
		}
	}
	return packets, offset, nil
}

// The text lines of the data packets, trailing newlines stripped.
func packetLines(packets []packet) []string {
	lines := []string{}
	for _, p := range packets {
		if p.kind == "data" {
			lines = append(lines, strings.TrimSuffix(string(p.payload), "\n"))
		}
	}
	return lines
}

// ---------------------------------------------------------------------------------------------
// packfiles

var typeCode = map[ObjType]byte{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeType = map[byte]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

// Encode one object's pack header (the type/size varint).
func packObjectHeader(objType ObjType, size int) []byte {
	out := []byte{}
	remaining := size
	first := (typeCode[objType] << 4) | byte(remaining&0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	out = append(out, first)
	return out
}

// Build a packfile of whole (never delta-compressed) objects, with its SHA-1 trailer.
func writePack(objects []*GitObj) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(objects)))
	buf.Write(header)
	for _, object := range objects {
		buf.Write(packObjectHeader(object.Type, len(object.Data)))
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		zw.Write(object.Data)
		zw.Close()
		buf.Write(zbuf.Bytes())
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// Inflate the zlib stream at `offset` in buffer (which ends where the stream must end);
// returns its bytes and the offset just past it.
func inflateAt(buffer []byte, offset int) ([]byte, int, error) {
	reader := bytes.NewReader(buffer[offset:])
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		zr.Close()
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(buffer) - offset - reader.Len()
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at %d consumed no bytes", offset)
	}
	return data, offset + consumed, nil
}

// Apply a git delta (copy/insert instructions) to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := uint64(0)
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value |= uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return int(value), nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copyOffset |= int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copySize |= int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) || written+copySize > len(out) {
				return nil, errors.New("delta copy is out of range")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) || written+literal > len(out) {
				return nil, errors.New("delta insert is out of range")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta produced the wrong size")
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   byte
	size       int
	data       []byte
	baseOffset int
	hasBaseOff bool
	baseOid    string
	hasBaseOid bool
}

// Read a packfile and resolve every object, OFS_DELTA and REF_DELTA included. A base a thin pack
// leaves out is taken from `resolveBase` (the repository's own store). Errors on any corruption.
func readPack(pack []byte, resolveBase func(oid string) *GitObj) ([]*GitObj, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:]))
	trailerStart := len(pack) - 20
	raw := []*rawPackEntry{}
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for index := 0; index < count; index++ {
		entryOffset := offset
		if offset >= trailerStart {
			return nil, errors.New("pack entry header is truncated")
		}
		b := pack[offset]
		offset++
		size := int(b & 0x0f)
		entryType := (b >> 4) & 0x07
		shift := uint(4)
		for b&0x80 != 0 {
			if offset >= trailerStart {
				return nil, errors.New("pack entry header is truncated")
			}
			b = pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		entry := &rawPackEntry{offset: entryOffset, typeCode: entryType}
		switch {
		case entryType == 6:
			if offset >= trailerStart {
				return nil, errors.New("pack entry header is truncated")
			}
			nb := pack[offset]
			offset++
			value := int(nb & 0x7f)
			for nb&0x80 != 0 {
				nb = pack[offset]
				offset++
				value = (value+1)*128 + int(nb&0x7f)
			}
			entry.baseOffset = entryOffset - value
			entry.hasBaseOff = true
		case entryType == 7:
			if offset+20 > trailerStart {
				return nil, errors.New("pack entry header is truncated")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			entry.hasBaseOid = true
			offset += 20
		default:
			if _, ok := codeType[entryType]; !ok {
				return nil, fmt.Errorf("unknown pack type code %d", entryType)
			}
		}
		data, next, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = data
		offset = next
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}

	resolved := map[int]*GitObj{}
	resolvedByOid := map[string]*GitObj{}
	resolving := map[int]bool{}
	var resolve func(entry *rawPackEntry) (*GitObj, error)
	resolve = func(entry *rawPackEntry) (*GitObj, error) {
		if cached, ok := resolved[entry.offset]; ok {
			return cached, nil
		}
		if resolving[entry.offset] {
			return nil, errors.New("delta cycle in pack")
		}
		resolving[entry.offset] = true
		var object *GitObj
		switch {
		case entry.typeCode == 6:
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			object = &GitObj{Type: base.Type, Data: data}
		case entry.typeCode == 7:
			base, ok := resolvedByOid[entry.baseOid]
			if !ok && resolveBase != nil {
				base = resolveBase(entry.baseOid)
			}
			if base == nil {
				// The base may sit later in the pack: resolve entries until one hashes to the wanted id.
				for _, candidate := range raw {
					if candidate == entry {
						continue
					}
					resolvedCandidate, err := resolve(candidate)
					if err != nil {
						return nil, err
					}
					resolvedByOid[hashObject(resolvedCandidate.Type, resolvedCandidate.Data)] = resolvedCandidate
					if hashObject(resolvedCandidate.Type, resolvedCandidate.Data) == entry.baseOid {
						base = resolvedCandidate
						break
					}
				}
			}
			if base == nil {
				return nil, errors.New("ref-delta base is not available")
			}
			data, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			object = &GitObj{Type: base.Type, Data: data}
		default:
			object = &GitObj{Type: codeType[entry.typeCode], Data: entry.data}
		}
		delete(resolving, entry.offset)
		resolved[entry.offset] = object
		return object, nil
	}
	objects := []*GitObj{}
	for _, entry := range raw {
		object, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
		// Register by id for later ref-delta bases: the id is the hash of the resolved object.
		resolvedByOid[hashObject(object.Type, object.Data)] = object
	}
	return objects, nil
}

// ---------------------------------------------------------------------------------------------
// reachability and fetch computation

// Every object reachable from `roots` (commits, their trees and parents; tags followed).
func collectClosure(repo *RepoRec, roots []string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		seen[oid] = true
		switch object.Type {
		case ObjCommit:
			commit := parseCommit(object.Data)
			stack = append(stack, commit.Tree)
			stack = append(stack, commit.Parents...)
		case ObjTag:
			if target := parseTagTarget(object.Data); target != "" {
				stack = append(stack, target)
			}
		case ObjTree:
			for _, entry := range parseTree(object.Data) {
				if entry.Mode != "160000" {
					stack = append(stack, entry.Oid)
				}
			}
		}
	}
	return seen
}

// A commit's tree closure: the tree, its sub-trees and their blobs.
func treeClosure(repo *RepoRec, commitOid string) map[string]bool {
	out := map[string]bool{}
	tree := commitTree(repo, commitOid)
	if tree == "" {
		return out
	}
	stack := []string{tree}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		object := repo.Objects[oid]
		if object == nil {
			continue
		}
		out[oid] = true
		if object.Type == ObjTree {
			for _, entry := range parseTree(object.Data) {
				if entry.Mode != "160000" {
					stack = append(stack, entry.Oid)
				}
			}
		}
	}
	return out
}

type fetchComputation struct {
	objects   []*GitObj
	shallow   []string
	unshallow []string
}

// What a fetch sends: the objects reachable from the wants that the client cannot already have,
// honouring haves (full history), the client's shallow boundary (commit + tree only) and a depth
// limit counted in commit generations from the wants.
func computeFetch(repo *RepoRec, wants, haves, shallowReq []string, depth *float64) fetchComputation {
	shallowCommits := map[string]bool{}
	for _, oid := range shallowReq {
		if commit := resolveCommit(repo, oid); commit != "" {
			shallowCommits[commit] = true
		}
	}
	fullHas := map[string]bool{}
	fullHasCommits := map[string]bool{}
	for _, have := range haves {
		commit := resolveCommit(repo, have)
		if commit != "" && shallowCommits[commit] {
			continue
		}
		if _, ok := repo.Objects[have]; !ok {
			continue
		}
		for oid := range collectClosure(repo, []string{have}) {
			fullHas[oid] = true
			if object := repo.Objects[oid]; object != nil && object.Type == ObjCommit {
				fullHasCommits[oid] = true
			}
		}
	}
	hasObjs := map[string]bool{}
	for oid := range fullHas {
		hasObjs[oid] = true
	}
	for commit := range shallowCommits {
		hasObjs[commit] = true
		for oid := range treeClosure(repo, commit) {
			hasObjs[oid] = true
		}
	}

	include := map[string]*GitObj{}
	includedCommits := map[string]bool{}
	addObject := func(oid string) {
		if hasObjs[oid] {
			return
		}
		if _, ok := include[oid]; ok {
			return
		}
		if object := repo.Objects[oid]; object != nil {
			include[oid] = object
		}
	}
	addTreeClosure := func(commitOid string) {
		for oid := range treeClosure(repo, commitOid) {
			addObject(oid)
		}
	}

	// Seed the walk: tag wants are included and peeled to their commit at generation 1.
	frontier := []string{}
	for _, want := range wants {
		object := repo.Objects[want]
		if object == nil {
			continue
		}
		switch object.Type {
		case ObjTag:
			addObject(want)
			if target := resolveCommit(repo, want); target != "" {
				frontier = append(frontier, target)
			}
		case ObjCommit:
			frontier = append(frontier, want)
		default:
			addObject(want)
		}
	}

	shallowOut := map[string]bool{}
	visited := map[string]bool{}
	generation := 1
	for len(frontier) > 0 {
		next := []string{}
		for _, oid := range frontier {
			if visited[oid] {
				continue
			}
			visited[oid] = true
			object := repo.Objects[oid]
			if object == nil || object.Type != ObjCommit {
				continue
			}
			if fullHasCommits[oid] {
				continue // the client holds this commit and all its history
			}
			parents := parseCommit(object.Data).Parents
			if shallowCommits[oid] {
				// The client holds the commit and its tree, but not its parents: keep walking.
				if depth == nil || float64(generation) < *depth {
					next = append(next, parents...)
				}
				continue
			}
			addObject(oid)
			includedCommits[oid] = true
			addTreeClosure(oid)
			if depth != nil && float64(generation) >= *depth {
				if len(parents) > 0 {
					shallowOut[oid] = true
				}
			} else {
				next = append(next, parents...)
			}
		}
		frontier = next
		generation++
	}

	unshallow := []string{}
	for commit := range shallowCommits {
		object := repo.Objects[commit]
		if object == nil || object.Type != ObjCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if includedCommits[parent] {
				unshallow = append(unshallow, commit)
				break
			}
		}
	}

	rank := map[ObjType]int{ObjCommit: 0, ObjTag: 1, ObjTree: 2, ObjBlob: 3}
	type entry struct {
		oid    string
		object *GitObj
	}
	entries := make([]entry, 0, len(include))
	for oid, object := range include {
		entries = append(entries, entry{oid: oid, object: object})
	}
	sort.Slice(entries, func(i, j int) bool {
		if rank[entries[i].object.Type] != rank[entries[j].object.Type] {
			return rank[entries[i].object.Type] < rank[entries[j].object.Type]
		}
		return entries[i].oid < entries[j].oid
	})
	objects := make([]*GitObj, len(entries))
	for i, e := range entries {
		objects[i] = e.object
	}
	shallow := make([]string, 0, len(shallowOut))
	for oid := range shallowOut {
		shallow = append(shallow, oid)
	}
	return fetchComputation{objects: objects, shallow: shallow, unshallow: unshallow}
}

// A small FIFO cache (the JS Maps this mirrors evict their oldest entry).
type fifoCache[V any] struct {
	limit int
	items map[string]V
	order []string
}

func newFifoCache[V any](limit int) *fifoCache[V] {
	return &fifoCache[V]{limit: limit, items: map[string]V{}}
}

func (c *fifoCache[V]) get(key string) (V, bool) {
	value, ok := c.items[key]
	return value, ok
}

func (c *fifoCache[V]) set(key string, value V) {
	if _, ok := c.items[key]; !ok {
		if len(c.items) >= c.limit && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.items, oldest)
		}
		c.order = append(c.order, key)
	}
	c.items[key] = value
}

// Built packs, keyed by everything that decides their bytes; objects are content-addressed.
var packCache = newFifoCache[[]byte](128)

func fetchPack(computation fetchComputation, cacheKey string) []byte {
	if cached, ok := packCache.get(cacheKey); ok {
		return cached
	}
	pack := writePack(computation.objects)
	packCache.set(cacheKey, pack)
	return pack
}

// ---------------------------------------------------------------------------------------------
// advertisements

const agentLine = "agent=makefun-forge/1.0"

func sortedRefs(repo *RepoRec) [][2]string {
	refs := make([][2]string, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, [2]string{name, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	return refs
}

// The commit a tag ref peels to, when it points at a tag object.
func peeledOf(repo *RepoRec, oid string) string {
	object := repo.Objects[oid]
	if object == nil || object.Type != ObjTag {
		return ""
	}
	return resolveCommit(repo, oid)
}

// The v0 ref advertisement lines (the first carries the capabilities after a NUL).
func refAdvertisement(repo *RepoRec, capabilities string) []byte {
	var buf bytes.Buffer
	refs := sortedRefs(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	headTip, hasHead := repo.Refs[headRef]
	first := true
	line := func(text string) {
		if first {
			buf.Write(pktText(text + "\x00" + capabilities + "\n"))
			first = false
		} else {
			buf.Write(pktText(text + "\n"))
		}
	}
	if len(refs) == 0 {
		line(ZeroOID + " capabilities^{}")
	} else {
		if hasHead {
			line(headTip + " HEAD")
		}
		for _, ref := range refs {
			line(ref[1] + " " + ref[0])
			if peeled := peeledOf(repo, ref[1]); peeled != "" && peeled != ref[1] {
				buf.Write(pktText(peeled + " " + ref[0] + "{}\n"))
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func advertisementV0(repo *RepoRec, service string) []byte {
	var capabilities string
	if service == "git-upload-pack" {
		capabilities = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow symref=HEAD:refs/heads/" + repo.DefaultBranch + " " + agentLine
	} else {
		capabilities = "report-status delete-refs side-band-64k ofs-delta " + agentLine
	}
	var buf bytes.Buffer
	buf.Write(pktText("# service=" + service + "\n"))
	buf.Write(pktFlush)
	buf.Write(refAdvertisement(repo, capabilities))
	return buf.Bytes()
}

func advertisementV2() []byte {
	var buf bytes.Buffer
	buf.Write(pktText("version 2\n"))
	buf.Write(pktText(agentLine + "\n"))
	buf.Write(pktText("ls-refs=unborn\n"))
	buf.Write(pktText("fetch=shallow\n"))
	buf.Write(pktText("object-format=sha1\n"))
	buf.Write(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------------------------
// upload-pack commands

// Wrap pack bytes in band-1 sideband packets (payloads capped under the pkt-line maximum).
func sidebandPackets(pack []byte) []byte {
	var buf bytes.Buffer
	chunkSize := pktMaxPayload - 1
	if len(pack) == 0 {
		buf.Write(pkt([]byte{1}))
		return buf.Bytes()
	}
	for offset := 0; offset < len(pack); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-offset+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[offset:end]...)
		buf.Write(pkt(chunk))
	}
	return buf.Bytes()
}

func handleLsRefs(repo *RepoRec, args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	prefixes := []string{}
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	headRef := "refs/heads/" + repo.DefaultBranch
	headTip, hasHead := repo.Refs[headRef]
	if matches("HEAD") {
		symref := ""
		if symrefs {
			symref = " symref-target:" + headRef
		}
		if hasHead {
			buf.Write(pktText(headTip + " HEAD" + symref + "\n"))
		} else if unborn {
			buf.Write(pktText("unborn HEAD" + symref + "\n"))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !matches(ref[0]) {
			continue
		}
		suffix := ""
		if peel {
			if peeled := peeledOf(repo, ref[1]); peeled != "" && peeled != ref[1] {
				suffix = " peeled:" + peeled
			}
		}
		buf.Write(pktText(ref[1] + " " + ref[0] + suffix + "\n"))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   *float64
	done    bool
}

func parseFetchArgs(args []string) fetchArgs {
	parsed := fetchArgs{wants: []string{}, haves: []string{}, shallow: []string{}}
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			parsed.wants = append(parsed.wants, arg[len("want "):])
		case strings.HasPrefix(arg, "have "):
			parsed.haves = append(parsed.haves, arg[len("have "):])
		case strings.HasPrefix(arg, "shallow "):
			parsed.shallow = append(parsed.shallow, arg[len("shallow "):])
		case strings.HasPrefix(arg, "deepen "):
			if value, err := strconv.ParseFloat(arg[len("deepen "):], 64); err == nil && value != 0 {
				parsed.depth = &value
			}
		case arg == "done":
			parsed.done = true
		}
	}
	return parsed
}

func sortedCopy(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}

func depthKey(depth *float64) string {
	if depth == nil {
		return "null"
	}
	return strconv.FormatFloat(*depth, 'f', -1, 64)
}

type fetchResult struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

// Whole fetch results, memoized. The key carries the repo's object count: objects are immutable
// and never deleted, so any push grows the count and stale entries simply stop matching — a
// repeated fetch with identical arguments against an unchanged store is a pure function, and
// the clone workload issues fifty of them.
var fetchCache = newFifoCache[fetchResult](256)

func runFetch(repo *RepoRec, args fetchArgs) fetchResult {
	key := strings.Join([]string{
		strconv.Itoa(repo.ID),
		strconv.Itoa(len(repo.Objects)),
		strings.Join(sortedCopy(args.wants), ","),
		strings.Join(sortedCopy(args.haves), ","),
		strings.Join(sortedCopy(args.shallow), ","),
		depthKey(args.depth),
	}, "|")
	if cached, ok := fetchCache.get(key); ok {
		return cached
	}
	computation := computeFetch(repo, args.wants, args.haves, args.shallow, args.depth)
	result := fetchResult{
		pack:      fetchPack(computation, key),
		shallow:   computation.shallow,
		unshallow: computation.unshallow,
	}
	fetchCache.set(key, result)
	return result
}

// Fully built fetch responses, under the same key discipline as fetchCache (object count guards staleness).
var fetchResponseCache = newFifoCache[[]byte](256)

func handleFetchV2(repo *RepoRec, args []string) []byte {
	parsed := parseFetchArgs(args)
	doneKey := "open"
	if parsed.done {
		doneKey = "done"
	}
	cacheKey := strings.Join([]string{
		strconv.Itoa(repo.ID),
		strconv.Itoa(len(repo.Objects)),
		strings.Join(sortedCopy(parsed.wants), ","),
		strings.Join(sortedCopy(parsed.haves), ","),
		strings.Join(sortedCopy(parsed.shallow), ","),
		depthKey(parsed.depth),
		doneKey,
	}, "|")
	if cached, ok := fetchResponseCache.get(cacheKey); ok {
		return cached
	}
	result := runFetch(repo, parsed)
	var buf bytes.Buffer
	if len(parsed.haves) > 0 {
		buf.Write(pktText("acknowledgments\n"))
		acks := []string{}
		for _, have := range parsed.haves {
			if _, ok := repo.Objects[have]; ok {
				acks = append(acks, have)
			}
		}
		if len(acks) == 0 {
			buf.Write(pktText("NAK\n"))
		} else {
			for _, ack := range acks {
				buf.Write(pktText("ACK " + ack + "\n"))
			}
			if parsed.done {
				buf.Write(pktText("ready\n"))
			}
		}
		buf.Write(pktFlush)
	}
	if parsed.depth != nil || len(parsed.shallow) > 0 {
		buf.Write(pktText("shallow-info\n"))
		for _, oid := range result.shallow {
			buf.Write(pktText("shallow " + oid + "\n"))
		}
		for _, oid := range result.unshallow {
			buf.Write(pktText("unshallow " + oid + "\n"))
		}
		buf.Write(pktFlush)
	}
	buf.Write(pktText("packfile\n"))
	buf.Write(sidebandPackets(result.pack))
	buf.Write(pktFlush)
	response := buf.Bytes()
	fetchResponseCache.set(cacheKey, response)
	return response
}

func handleFetchV0(repo *RepoRec, packets []packet) []byte {
	// Two sections: wants/shallow/deepen up to the first flush, haves/done up to the second.
	args := []string{}
	section := 0
	for _, p := range packets {
		if p.kind == "flush" {
			section++
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if section == 0 && strings.HasPrefix(line, "want ") {
			rest := line[len("want "):]
			oid := rest
			if space := strings.IndexByte(rest, ' '); space >= 0 {
				oid = rest[:space]
			}
			args = append(args, "want "+oid)
		} else {
			args = append(args, line)
		}
	}
	parsed := parseFetchArgs(args)
	result := runFetch(repo, parsed)
	var buf bytes.Buffer
	for _, oid := range result.shallow {
		buf.Write(pktText("shallow " + oid + "\n"))
	}
	for _, oid := range result.unshallow {
		buf.Write(pktText("unshallow " + oid + "\n"))
	}
	buf.Write(pktText("NAK\n"))
	buf.Write(sidebandPackets(result.pack))
	buf.Write(pktFlush)
	return buf.Bytes()
}

func handleUploadPack(repo *RepoRec, input GitRequest) GitResult {
	ok := func(body []byte) GitResult {
		return GitResult{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: body}
	}
	packets, _, err := parsePackets(input.Raw, 0)
	if err != nil {
		return GitResult{Status: 400, ContentType: "text/plain", Body: []byte("invalid pkt-line\n")}
	}
	wantsV2 := strings.Contains(input.GitProtocol, "version=2")
	if !wantsV2 {
		return ok(handleFetchV0(repo, packets))
	}
	// v2: `command=<name>`, capability lines, a delim, argument lines, a flush.
	lines := []string{}
	command := ""
	afterDelim := false
	for _, p := range packets {
		if p.kind == "delim" {
			afterDelim = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if !afterDelim {
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
			continue
		}
		lines = append(lines, line)
	}
	if command == "ls-refs" {
		return ok(handleLsRefs(repo, lines))
	}
	if command == "fetch" {
		return ok(handleFetchV2(repo, lines))
	}
	return GitResult{Status: 400, ContentType: "text/plain", Body: []byte("unknown command " + command + "\n")}
}

// ---------------------------------------------------------------------------------------------
// receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(repo *RepoRec, input GitRequest) GitResult {
	resultType := "application/x-git-receive-pack-result"
	// Commands are pkt-lines up to the first flush; everything after it is the packfile.
	offset := 0
	commandLines := []string{}
	parseFailed := false
	for {
		if offset+4 > len(input.Raw) {
			parseFailed = true
			break
		}
		header := string(input.Raw[offset : offset+4])
		if !isHex4(header) {
			parseFailed = true
			break
		}
		length, _ := strconv.ParseInt(header, 16, 32)
		if length == 0 {
			offset += 4
			break
		}
		if length < 4 || offset+int(length) > len(input.Raw) {
			parseFailed = true
			break
		}
		commandLines = append(commandLines, strings.TrimSuffix(string(input.Raw[offset+4:offset+int(length)]), "\n"))
		offset += int(length)
	}
	if parseFailed {
		return GitResult{Status: 400, ContentType: "text/plain", Body: []byte("invalid pkt-line\n")}
	}
	packBytes := input.Raw[offset:]

	capabilities := map[string]bool{}
	commands := []pushCommand{}
	for index, line := range commandLines {
		text := line
		if index == 0 {
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				for _, cap := range strings.Split(text[nul+1:], " ") {
					if cap != "" {
						capabilities[cap] = true
					}
				}
				text = text[:nul]
			}
		}
		firstSpace := strings.IndexByte(text, ' ')
		if firstSpace < 0 {
			continue
		}
		rest := text[firstSpace+1:]
		secondSpace := strings.IndexByte(rest, ' ')
		if secondSpace < 0 {
			continue
		}
		commands = append(commands, pushCommand{
			oldOid: text[:firstSpace],
			newOid: rest[:secondSpace],
			ref:    rest[secondSpace+1:],
		})
	}

	unpackError := ""
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, func(oid string) *GitObj { return repo.Objects[oid] })
		if err != nil {
			unpackError = err.Error()
		} else {
			for _, object := range objects {
				objPut(repo, object.Type, object.Data)
			}
		}
	}

	type report struct {
		ref    string
		ok     bool
		reason string
	}
	reports := []report{}
	for _, command := range commands {
		fail := func(reason string) {
			reports = append(reports, report{ref: command.ref, ok: false, reason: reason})
		}
		current, hasCurrent := repo.Refs[command.ref]
		if !hasCurrent {
			current = ZeroOID
		}
		if unpackError != "" {
			fail("unpack failed")
			continue
		}
		if command.newOid == ZeroOID {
			// A delete: the ref must be where the command says it is (or already gone).
			if current != command.oldOid {
				fail("stale ref")
			} else {
				delete(repo.Refs, command.ref)
				reports = append(reports, report{ref: command.ref, ok: true})
			}
			continue
		}
		object := repo.Objects[command.newOid]
		if object == nil {
			fail("missing object")
			continue
		}
		if strings.HasPrefix(command.ref, "refs/heads/") && resolveCommit(repo, command.newOid) == "" {
			fail("not a commit")
			continue
		}
		if command.oldOid == ZeroOID {
			if current != ZeroOID {
				fail("already exists")
				continue
			}
		} else {
			if current != command.oldOid {
				fail("stale ref")
				continue
			}
			if !isAncestor(repo, command.oldOid, command.newOid) {
				fail("non-fast-forward")
				continue
			}
		}
		repo.Refs[command.ref] = command.newOid
		reports = append(reports, report{ref: command.ref, ok: true})
	}

	if !capabilities["report-status"] && !capabilities["report-status-v2"] {
		return GitResult{Status: 200, ContentType: resultType, Body: []byte{}}
	}
	var stream bytes.Buffer
	if unpackError == "" {
		stream.Write(pktText("unpack ok\n"))
	} else {
		stream.Write(pktText("unpack error: " + unpackError + "\n"))
	}
	for _, r := range reports {
		if r.ok {
			stream.Write(pktText("ok " + r.ref + "\n"))
		} else {
			stream.Write(pktText("ng " + r.ref + " " + r.reason + "\n"))
		}
	}
	stream.Write(pktFlush)
	reportStream := stream.Bytes()
	if !capabilities["side-band-64k"] {
		return GitResult{Status: 200, ContentType: resultType, Body: reportStream}
	}
	var wrapped bytes.Buffer
	wrapped.Write(sidebandPackets(reportStream))
	wrapped.Write(pktFlush)
	return GitResult{Status: 200, ContentType: resultType, Body: wrapped.Bytes()}
}

// ---------------------------------------------------------------------------------------------
// the handler

func handleGitRequest(forge *Forge, input GitRequest) GitResult {
	notFound := func() GitResult {
		return GitResult{Status: 404, ContentType: "text/plain", Body: []byte("not found\n")}
	}
	if input.Service == "info/refs" {
		service := input.Query.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return GitResult{Status: 400, ContentType: "text/plain", Body: []byte("service not understood\n")}
		}
		repo := forge.GetRepo(input.Owner, input.Repo)
		if repo == nil {
			return notFound()
		}
		if service == "git-receive-pack" {
			return GitResult{
				Status:      200,
				ContentType: "application/x-git-receive-pack-advertisement",
				Body:        advertisementV0(repo, "git-receive-pack"),
				Headers:     map[string]string{"cache-control": "no-cache"},
			}
		}
		wantsV2 := strings.Contains(input.GitProtocol, "version=2")
		body := advertisementV0(repo, "git-upload-pack")
		if wantsV2 {
			body = advertisementV2()
		}
		return GitResult{
			Status:      200,
			ContentType: "application/x-git-upload-pack-advertisement",
			Body:        body,
			Headers:     map[string]string{"cache-control": "no-cache"},
		}
	}
	repo := forge.GetRepo(input.Owner, input.Repo)
	if repo == nil {
		return notFound()
	}
	if input.Service == "git-upload-pack" {
		return handleUploadPack(repo, input)
	}
	if input.Service == "git-receive-pack" {
		return handleReceivePack(repo, input)
	}
	return notFound()
}
