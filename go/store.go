// The forge's in-memory store: git objects, packs, trees, diffs and merges,
// plus the REST/GraphQL/MCP resources (repositories, issues, pull requests,
// labels, comments) that all four surfaces share. One process, no disk, no
// clock: every timestamp derives from a deterministic counter, so the same
// operation sequence yields the same state on every run.
//
// Git model (SHA-1, pack v2, no deltas on write):
//   - objects are {type, data}; the id is SHA-1 over "<type> <len>\0" + data.
//   - packs are read with full OFS_DELTA/REF_DELTA resolution (a client may send either);
//     packs are written delta-free (plain objects), which the suite accepts.
//   - server commits (contents API, merges) are real git objects in the same store as pushes.
//
// Performance notes (vs the Node original this ports):
//   - objects carry their oid, so writePack sorts on the stored oid instead
//     of re-hashing per comparison.
//   - pack inflate reuses pooled zlib readers; the exact compressed length of
//     each entry is measured with a byte-at-a-time counting reader, so no
//     re-scan is ever needed.
//   - pack deflate reuses pooled zlib writers.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ZeroOID is the all-zero object id.
const ZeroOID = zeroOID

type gitObjectType string

const (
	objCommit gitObjectType = "commit"
	objTree   gitObjectType = "tree"
	objBlob   gitObjectType = "blob"
	objTag    gitObjectType = "tag"
)

// GitObject is one stored git object. Oid is computed once, at ingestion.
type GitObject struct {
	Type gitObjectType
	Data []byte
	Oid  string
}

var typeCode = map[gitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]gitObjectType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

// isObjectID reports whether value is a lowercase 40-hex object id.
func isObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var sha1Pool = sync.Pool{New: func() any { return sha1.New() }}

// hashObject is git's object id: SHA-1 over `<type> <size>\0` + the object's bytes.
func hashObject(typ gitObjectType, data []byte) string {
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	var hdr []byte
	hdr = append(hdr, string(typ)...)
	hdr = append(hdr, ' ')
	hdr = strconv.AppendInt(hdr, int64(len(data)), 10)
	hdr = append(hdr, 0)
	h.Write(hdr)
	h.Write(data)
	sum := h.Sum(nil)
	sha1Pool.Put(h)
	return hex.EncodeToString(sum)
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// parseTree parses a tree object into its entries, in the object's stored order.
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

// compareTreeNames is git's tree-entry sort: byte order of the name, a
// directory compared as if it ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

// buildTree builds a tree object from entries; entries are sorted the way git sorts them.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var out []byte
	for _, e := range sorted {
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		raw, _ := hex.DecodeString(e.Oid)
		out = append(out, raw...)
	}
	return out
}

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// parseCommit parses a commit object.
func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = string(data[blank+2:])
	}
	var view CommitView
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		key, value := line, ""
		if space >= 0 {
			key, value = line[:space], line[space+1:]
		}
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
	view.Message = message
	return view
}

// buildCommit builds a commit object's bytes.
func buildCommit(tree string, parents []string, message, author, committer string, timestamp int64, timezone string) []byte {
	if timestamp == 0 {
		timestamp = 1700000000
	}
	if timezone == "" {
		timezone = "+0000"
	}
	if author == "" {
		author = "Conformance <conformance@make.fun>"
	}
	if committer == "" {
		committer = "Conformance <conformance@make.fun>"
	}
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	fmt.Fprintf(&b, "author %s %d %s\n", author, timestamp, timezone)
	fmt.Fprintf(&b, "committer %s %d %s\n", committer, timestamp, timezone)
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Type    gitObjectType
	Tag     string
	Tagger  string
	Message string
}

// parseTag parses an annotated tag object.
func parseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = string(data[blank+2:])
	}
	headers := make(map[string]string)
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		if space < 0 {
			continue
		}
		headers[line[:space]] = line[space+1:]
	}
	typ := gitObjectType(headers["type"])
	switch typ {
	case objCommit, objTree, objBlob, objTag:
	default:
		typ = objCommit
	}
	return TagView{Object: headers["object"], Type: typ, Tag: headers["tag"], Tagger: headers["tagger"], Message: message}
}

// ---------------------------------------------------------------- packfiles

// ObjectLookup resolves an oid to its object, or nil.
type ObjectLookup func(oid string) *GitObject

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = ((value + 1) * 128) + int(b&0x7f)
	}
	return value, offset, nil
}

// countingReader hands out at most one byte per Read, so a zlib reader built
// on it consumes exactly the stream's bytes — no over-read into the next
// entry — and pos is the exact compressed length when the stream ends.
type countingReader struct {
	b   []byte
	pos int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.pos]
	r.pos++
	return 1, nil
}

// ReadByte lets compress/flate read the stream byte by byte directly: without
// it flate wraps the reader in a bufio that refills (one Read call) per byte,
// which measured 4.2x slower on a 1 KiB object (205 us vs 49 us). pos stays
// exact either way, so the offset past the stream is unchanged.
func (r *countingReader) ReadByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

// zlibReaderPool reuses zlib readers (each holds a flate decompressor).
var zlibReaderPool = sync.Pool{New: func() any {
	zr, _ := zlib.NewReader(bytes.NewReader(nil))
	if rs, ok := zr.(zlib.Resetter); ok {
		return rs
	}
	return nil
}}

var zlibWriterPool = sync.Pool{New: func() any {
	zw, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return zw
}}

// inflateAt inflates one zlib stream at offset; returns its bytes and the
// offset just past the stream (including the adler32 trailer).
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	cr := &countingReader{b: buf[offset:]}
	var zr zlib.Resetter
	if pooled := zlibReaderPool.Get(); pooled != nil {
		if rs, ok := pooled.(zlib.Resetter); ok {
			zr = rs
		}
	}
	if zr == nil {
		fresh, err := zlib.NewReader(cr)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
		}
		defer fresh.Close()
		data, err = io.ReadAll(fresh)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
		}
		if cr.pos <= 0 {
			return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
		}
		return data, offset + cr.pos, nil
	}
	if err := zr.Reset(cr, nil); err != nil {
		zlibReaderPool.Put(zr)
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	rc := zr.(io.ReadCloser)
	data, err = io.ReadAll(rc)
	rc.Close()
	zlibReaderPool.Put(zr)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if cr.pos <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + cr.pos, nil
}

// applyDelta applies a git delta (the copy/insert instruction stream) to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
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
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
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
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// verifyPackChecksum reports whether the pack's 20-byte trailer matches SHA-1
// over the bytes before it.
func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 32 {
		return false
	}
	actual := pack[len(pack)-20:]
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(actual, sum[:])
}

// readPack reads a packfile (version 2 or 3) and resolves every object,
// including OFS_DELTA and REF_DELTA. resolveBase supplies a base object a
// thin pack leaves out. It returns an error on any corruption.
func readPack(pack []byte, resolveBase ObjectLookup) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	if !verifyPackChecksum(pack) {
		return nil, errors.New("pack trailer checksum does not match its bytes")
	}

	type rawEntry struct {
		offset       int
		typeCode     int
		size         int
		data         []byte
		baseOffset   int
		hasBaseOff   bool
		baseOid      string
		resolvedType gitObjectType
		resolved     []byte
		oid          string
	}

	raw := make([]*rawEntry, 0, declaredCount)
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		tc := int((first >> 4) & 0x07)
		entry := &rawEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, next2, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = next2
			if base <= 0 || base > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset", entryOffset)
			}
			entry.baseOffset = entryOffset - base
			entry.hasBaseOff = true
		} else if tc == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, tc)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	// Fast path: packs without deltas (every benchmark push) need no
	// resolution machinery — hash each entry directly. Identical result to
	// the resolver below for delta-free packs (first object wins on a
	// duplicate oid), minus three map allocations and the recursion.
	hasDelta := false
	for _, entry := range raw {
		if entry.typeCode == 6 || entry.typeCode == 7 {
			hasDelta = true
			break
		}
	}
	if !hasDelta {
		objects := make(map[string]*GitObject, len(raw))
		for _, entry := range raw {
			typ := codeType[entry.typeCode]
			oid := hashObject(typ, entry.data)
			if _, ok := objects[oid]; !ok {
				objects[oid] = &GitObject{Type: typ, Data: entry.data, Oid: oid}
			}
		}
		return objects, nil
	}

	byOffset := make(map[int]*rawEntry, len(raw))
	for _, entry := range raw {
		byOffset[entry.offset] = entry
	}
	objects := make(map[string]*GitObject, len(raw))
	resolving := make(map[int]bool)
	resolvedByOid := make(map[string]*rawEntry)

	var resolve func(entry *rawEntry) (gitObjectType, []byte, string, error)
	resolve = func(entry *rawEntry) (gitObjectType, []byte, string, error) {
		if entry.resolved != nil && entry.oid != "" {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ gitObjectType
		var data []byte
		var err error
		switch entry.typeCode {
		case 6:
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				err = fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			} else {
				var baseType gitObjectType
				var baseData []byte
				baseType, baseData, _, err = resolve(baseEntry)
				if err == nil {
					typ = baseType
					data, err = applyDelta(baseData, entry.data)
				}
			}
		case 7:
			wanted := entry.baseOid
			if baseEntry := resolvedByOid[wanted]; baseEntry != nil {
				var baseType gitObjectType
				var baseData []byte
				baseType, baseData, _, err = resolve(baseEntry)
				if err == nil {
					typ = baseType
					data, err = applyDelta(baseData, entry.data)
				}
			} else if resolveBase != nil {
				external := resolveBase(wanted)
				if external == nil {
					err = fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", wanted)
				} else {
					typ = external.Type
					data, err = applyDelta(external.Data, entry.data)
				}
			} else {
				err = fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", wanted)
			}
		default:
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		if err != nil {
			delete(resolving, entry.offset)
			return "", nil, "", err
		}
		oid := hashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}

	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data, Oid: oid}
		}
	}
	return objects, nil
}

func writePackObjectHeader(typ gitObjectType, size int) []byte {
	code := typeCode[typ]
	var bytesOut []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining /= 16
	for remaining > 0 {
		bytesOut = append(bytesOut, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	bytesOut = append(bytesOut, first)
	return bytesOut
}

// writePack builds a packfile from plain objects (no deltas), in oid order.
// The trailer is SHA-1 over the rest. Objects are sorted on their stored oid.
func writePack(objects []*GitObject) []byte {
	list := make([]*GitObject, len(objects))
	copy(list, objects)
	sort.Slice(list, func(i, j int) bool { return list[i].Oid < list[j].Oid })
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(list)))
	body.Write(header)
	zw := zlibWriterPool.Get().(*zlib.Writer)
	for _, object := range list {
		body.Write(writePackObjectHeader(object.Type, len(object.Data)))
		zw.Reset(&body)
		zw.Write(object.Data)
		zw.Close()
	}
	zlibWriterPool.Put(zw)
	pack := body.Bytes()
	sum := sha1.Sum(pack)
	out := make([]byte, 0, len(pack)+20)
	out = append(out, pack...)
	out = append(out, sum[:]...)
	return out
}

// ---------------------------------------------------------------- reachability

// ReachResult is what collectReachable found.
type ReachResult struct {
	// Every object id to send (commits, tags, trees, blobs).
	Objects map[string]bool
	// The new shallow boundary: included commits whose parents were cut off by depth.
	Shallow []string
	// Every commit id the walk visited, including have-commits it traversed through.
	Visited map[string]bool
}

// collectReachable returns the objects a fetch must send for roots, minus
// what exclude (the client's haves) already holds, cut at depth generations
// from the roots (root = generation 1). A commit's tree closure is always
// complete; only the commit walk is depth-limited.
func collectReachable(lookup ObjectLookup, roots []string, depth int, hasDepth bool, exclude, clientShallow map[string]bool) ReachResult {
	if exclude == nil {
		exclude = map[string]bool{}
	}
	if clientShallow == nil {
		clientShallow = map[string]bool{}
	}
	isCompleteHave := func(oid string) bool { return exclude[oid] && !clientShallow[oid] }
	// The objects the client already holds: the closure of its haves.
	held := make(map[string]bool)
	heldTree := func(treeOid string) {
		stack := []string{treeOid}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if held[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil || object.Type != objTree {
				continue
			}
			held[oid] = true
			entries, err := parseTree(object.Data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.Mode == "160000" {
					continue
				}
				child := lookup(entry.Oid)
				if child == nil {
					continue
				}
				if child.Type == objTree {
					stack = append(stack, entry.Oid)
				} else {
					held[entry.Oid] = true
				}
			}
		}
	}
	var completeQueue []string
	for have := range exclude {
		if !clientShallow[have] {
			completeQueue = append(completeQueue, have)
		}
	}
	seenComplete := make(map[string]bool)
	for len(completeQueue) > 0 {
		oid := completeQueue[len(completeQueue)-1]
		completeQueue = completeQueue[:len(completeQueue)-1]
		if seenComplete[oid] {
			continue
		}
		seenComplete[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		held[oid] = true
		commit := parseCommit(object.Data)
		heldTree(commit.Tree)
		for _, parent := range commit.Parents {
			completeQueue = append(completeQueue, parent)
		}
	}
	for boundary := range clientShallow {
		if !exclude[boundary] {
			continue
		}
		if held[boundary] {
			continue
		}
		object := lookup(boundary)
		if object == nil || object.Type != objCommit {
			continue
		}
		held[boundary] = true
		heldTree(parseCommit(object.Data).Tree)
	}
	objects := make(map[string]bool)
	var shallow []string
	generation := make(map[string]int)
	var queue []string
	enqueueCommit := func(oid string, gen int) {
		if _, ok := generation[oid]; ok {
			return
		}
		if isCompleteHave(oid) {
			return
		}
		if hasDepth && gen > depth {
			return
		}
		generation[oid] = gen
		queue = append(queue, oid)
	}
	includeTree := func(treeOid string) {
		stack := []string{treeOid}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if objects[oid] {
				continue
			}
			if held[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil || object.Type != objTree {
				continue
			}
			objects[oid] = true
			entries, err := parseTree(object.Data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.Mode == "160000" {
					continue
				}
				child := lookup(entry.Oid)
				if child == nil {
					continue
				}
				if child.Type == objTree {
					stack = append(stack, entry.Oid)
				} else if !held[entry.Oid] {
					objects[entry.Oid] = true
				}
			}
		}
	}
	for _, root := range roots {
		if !isObjectID(root) {
			continue
		}
		object := lookup(root)
		if object == nil {
			continue
		}
		switch object.Type {
		case objCommit:
			enqueueCommit(root, 1)
		case objTag:
			if !exclude[root] && !held[root] {
				objects[root] = true
			}
			target := parseTag(object.Data).Object
			targetObject := lookup(target)
			if targetObject == nil {
				continue
			}
			switch targetObject.Type {
			case objCommit:
				enqueueCommit(target, 1)
			case objTree:
				if !exclude[target] && !held[target] {
					includeTree(target)
				}
			default:
				if !exclude[target] && !held[target] {
					objects[target] = true
				}
			}
		case objTree:
			if !exclude[root] && !held[root] {
				includeTree(root)
			}
		default:
			if !exclude[root] && !held[root] {
				objects[root] = true
			}
		}
	}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		gen := generation[oid]
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		isHave := exclude[oid]
		commit := parseCommit(object.Data)
		if !isHave {
			objects[oid] = true
			includeTree(commit.Tree)
		}
		atBoundary := hasDepth && gen == depth
		if !isHave && atBoundary && len(commit.Parents) > 0 {
			cut := false
			for _, p := range commit.Parents {
				if !exclude[p] && !objects[p] && lookup(p) != nil {
					cut = true
					break
				}
			}
			if cut {
				shallow = append(shallow, oid)
			}
		}
		if atBoundary {
			continue
		}
		for _, parent := range commit.Parents {
			enqueueCommit(parent, gen+1)
		}
	}
	visited := make(map[string]bool, len(generation))
	for k := range generation {
		visited[k] = true
	}
	return ReachResult{Objects: objects, Shallow: shallow, Visited: visited}
}

// isAncestor reports whether ancestor is reachable from descendant by following parents.
func isAncestor(lookup ObjectLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{descendant}
	steps := 0
	for len(queue) > 0 && steps < 1000000 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		steps++
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// mergeBase returns the nearest common ancestor of two commits
// (BFS from b into a's ancestor set).
func mergeBase(lookup ObjectLookup, a, b string) string {
	ancestorsA := make(map[string]bool)
	queueA := []string{a}
	steps := 0
	for len(queueA) > 0 && steps < 1000000 {
		oid := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		steps++
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queueA = append(queueA, parent)
		}
	}
	seenB := make(map[string]bool)
	queueB := []string{b}
	steps = 0
	for head := 0; head < len(queueB) && steps < 1000000; head++ {
		oid := queueB[head]
		steps++
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ancestorsA[oid] {
			return oid
		}
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queueB = append(queueB, parent)
		}
	}
	return ""
}

// listCommits returns every commit reachable from tip, newest first
// (BFS by generation, parents after children).
func listCommits(lookup ObjectLookup, tip string, limit int) []string {
	var out []string
	seen := make(map[string]bool)
	queue := []string{tip}
	for head := 0; head < len(queue) && len(out) < limit; head++ {
		oid := queue[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		out = append(out, oid)
		for _, parent := range parseCommit(object.Data).Parents {
			queue = append(queue, parent)
		}
	}
	return out
}

// ---------------------------------------------------------------- tree operations

type treeFound struct {
	entry TreeEntry
	oid   string
}

// treeGetPath returns the entry at segments in the tree, or nil when any step is missing.
func treeGetPath(lookup ObjectLookup, treeOid string, segments []string) *treeFound {
	current := treeOid
	for i, seg := range segments {
		object := lookup(current)
		if object == nil || object.Type != objTree {
			return nil
		}
		entries, err := parseTree(object.Data)
		if err != nil {
			return nil
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(segments)-1 {
			return &treeFound{entry: *found, oid: found.Oid}
		}
		if found.Mode != "40000" {
			return nil
		}
		current = found.Oid
	}
	return nil
}

// treeListDir returns the entries of the directory at segments
// (empty = the root), or nil when it is not a tree.
func treeListDir(lookup ObjectLookup, treeOid string, segments []string) []TreeEntry {
	if len(segments) == 0 {
		object := lookup(treeOid)
		if object == nil || object.Type != objTree {
			return nil
		}
		entries, err := parseTree(object.Data)
		if err != nil {
			return nil
		}
		return entries
	}
	found := treeGetPath(lookup, treeOid, segments)
	if found == nil || found.entry.Mode != "40000" {
		return nil
	}
	object := lookup(found.oid)
	if object == nil || object.Type != objTree {
		return nil
	}
	entries, err := parseTree(object.Data)
	if err != nil {
		return nil
	}
	return entries
}

type flatEntry struct {
	oid  string
	mode string
}

// flattenTree returns every file in the tree, path -> {oid, mode}, directories expanded.
func flattenTree(lookup ObjectLookup, treeOid string) map[string]flatEntry {
	out := make(map[string]flatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		object := lookup(oid)
		if object == nil || object.Type != objTree {
			return
		}
		entries, err := parseTree(object.Data)
		if err != nil {
			return
		}
		for _, entry := range entries {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.Oid, path)
			} else if entry.Mode != "160000" {
				out[path] = flatEntry{oid: entry.Oid, mode: entry.Mode}
			}
		}
	}
	walk(treeOid, "")
	return out
}

// storeObjects is the sink treeSetPath/treeFromMap write new trees to.
type storeObjects interface {
	put(typ gitObjectType, data []byte) string
}

// treeSetPath returns a tree with segments set to blobOid (or deleted when
// blobOid is ""), every other entry kept. Intermediate directories are
// created; directories left empty by a delete are pruned. treeOid may be ""
// for an empty tree.
func treeSetPath(lookup ObjectLookup, store storeObjects, treeOid string, segments []string, blobOid string) string {
	head := segments[0]
	rest := segments[1:]
	var entries []TreeEntry
	if treeOid != "" {
		if object := lookup(treeOid); object != nil {
			entries, _ = parseTree(object.Data)
		}
	}
	var kept []TreeEntry
	for _, e := range entries {
		if e.Name != head {
			kept = append(kept, e)
		}
	}
	if len(rest) == 0 {
		if blobOid != "" {
			kept = append(kept, TreeEntry{Mode: "100644", Name: head, Oid: blobOid})
		}
	} else {
		var existing *TreeEntry
		for i := range entries {
			if entries[i].Name == head {
				existing = &entries[i]
				break
			}
		}
		if existing != nil && existing.Mode != "40000" {
			panic("path segment " + head + " is a file, not a directory")
		}
		childOid := ""
		if existing != nil {
			childOid = existing.Oid
		}
		childOid = treeSetPath(lookup, store, childOid, rest, blobOid)
		var childEntries []TreeEntry
		if object := lookup(childOid); object != nil {
			childEntries, _ = parseTree(object.Data)
		}
		if len(childEntries) > 0 {
			kept = append(kept, TreeEntry{Mode: "40000", Name: head, Oid: childOid})
		}
	}
	return store.put(objTree, buildTree(kept))
}

// treeFromMap builds a tree from a flat path -> blob map (nested directories created as needed).
func treeFromMap(lookup ObjectLookup, store storeObjects, files map[string]flatEntry) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	tree := ""
	for _, path := range paths {
		tree = treeSetPath(lookup, store, tree, strings.Split(path, "/"), files[path].oid)
	}
	if tree == "" {
		return store.put(objTree, buildTree(nil))
	}
	return tree
}

// ---------------------------------------------------------------- diffs and merges

// DiffEntry is one file-level diff record.
type DiffEntry struct {
	Path      string
	Status    string // "added" | "removed" | "modified"
	OldOid    string
	NewOid    string
	Additions int
	Deletions int
	Changes   int
}

func lineCount(content string) int {
	if content == "" {
		return 0
	}
	newlines := strings.Count(content, "\n")
	if strings.HasSuffix(content, "\n") {
		return newlines
	}
	return newlines + 1
}

// diffTrees returns the file-level diff between two trees ("" = the empty tree).
func diffTrees(lookup ObjectLookup, baseTree, headTree string) []DiffEntry {
	var base, head map[string]flatEntry
	if baseTree == "" {
		base = map[string]flatEntry{}
	} else {
		base = flattenTree(lookup, baseTree)
	}
	if headTree == "" {
		head = map[string]flatEntry{}
	} else {
		head = flattenTree(lookup, headTree)
	}
	pathSet := make(map[string]bool)
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []DiffEntry
	for _, path := range paths {
		oldEntry, hasOld := base[path]
		newEntry, hasNew := head[path]
		if hasOld && hasNew && oldEntry.oid == newEntry.oid {
			continue
		}
		oldText := ""
		if hasOld {
			if o := lookup(oldEntry.oid); o != nil {
				oldText = string(o.Data)
			}
		}
		newText := ""
		if hasNew {
			if o := lookup(newEntry.oid); o != nil {
				newText = string(o.Data)
			}
		}
		if !hasOld {
			additions := lineCount(newText)
			out = append(out, DiffEntry{Path: path, Status: "added", NewOid: newEntry.oid, Additions: additions, Changes: additions})
		} else if !hasNew {
			deletions := lineCount(oldText)
			out = append(out, DiffEntry{Path: path, Status: "removed", OldOid: oldEntry.oid, Deletions: deletions, Changes: deletions})
		} else {
			additions := lineCount(newText)
			deletions := lineCount(oldText)
			out = append(out, DiffEntry{Path: path, Status: "modified", OldOid: oldEntry.oid, NewOid: newEntry.oid, Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	return out
}

// MergeOutcome is the result of a three-way merge.
type MergeOutcome struct {
	Tree      string // "" when conflicts were found
	Conflicts []string
}

// threeWayMerge merges headTree into baseTree over their mergeBaseTree
// ("" = empty). A path both sides changed differently is a conflict.
func threeWayMerge(lookup ObjectLookup, store storeObjects, baseTree, headTree, mergeBaseTree string) MergeOutcome {
	var base, head, ancestor map[string]flatEntry
	if baseTree == "" {
		base = map[string]flatEntry{}
	} else {
		base = flattenTree(lookup, baseTree)
	}
	if headTree == "" {
		head = map[string]flatEntry{}
	} else {
		head = flattenTree(lookup, headTree)
	}
	if mergeBaseTree == "" {
		ancestor = map[string]flatEntry{}
	} else {
		ancestor = flattenTree(lookup, mergeBaseTree)
	}
	pathSet := make(map[string]bool)
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	for p := range ancestor {
		pathSet[p] = true
	}
	merged := make(map[string]flatEntry)
	var conflicts []string
	for path := range pathSet {
		a, hasA := ancestor[path]
		aOid := ""
		if hasA {
			aOid = a.oid
		}
		o, hasO := base[path]
		oOid := ""
		if hasO {
			oOid = o.oid
		}
		t, hasT := head[path]
		tOid := ""
		if hasT {
			tOid = t.oid
		}
		if oOid == tOid {
			if hasO {
				merged[path] = o
			}
			continue
		}
		if oOid == aOid {
			if hasT {
				merged[path] = t
			}
			continue
		}
		if tOid == aOid {
			if hasO {
				merged[path] = o
			}
			continue
		}
		conflicts = append(conflicts, path)
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return MergeOutcome{Tree: "", Conflicts: conflicts}
	}
	return MergeOutcome{Tree: treeFromMap(lookup, store, merged)}
}

// ---------------------------------------------------------------- global (Relay) ids

var globalIDCache = make(map[string]string)

// globalID is an opaque global id: base64 of `Type:key`.
func globalID(typ, key string) string {
	text := typ + ":" + key
	if cached, ok := globalIDCache[text]; ok {
		return cached
	}
	cached := base64.StdEncoding.EncodeToString([]byte(text))
	globalIDCache[text] = cached
	return cached
}

type parsedGlobalID struct {
	typ string
	key string
}

// parseGlobalID parses a global id back into its type and key, or nil when it is not one of ours.
func parseGlobalID(id string) *parsedGlobalID {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	text := string(raw)
	colon := strings.Index(text, ":")
	if colon <= 0 {
		return nil
	}
	typ := text[:colon]
	key := text[colon+1:]
	if key == "" {
		return nil
	}
	if base64.StdEncoding.EncodeToString([]byte(text)) != id {
		return nil
	}
	return &parsedGlobalID{typ: typ, key: key}
}

// ---------------------------------------------------------------- the forge store

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Comment is an issue comment.
type Comment struct {
	ID   int
	Body string
	Seq  int
}

// Issue is a repository issue.
type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []*Label
	Comments   []*Comment
	Seq        int
	UpdatedSeq int
}

// Pull is a repository pull request.
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string // "" when never merged
	HasMergedAt    bool
	MergeCommitSha string // "" when never merged
	HasMergeSha    bool
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Seq            int
	UpdatedSeq     int
}

// Repo is one repository: its git objects and refs plus its REST resources.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Seq           int
	Objects       map[string]*GitObject
	Refs          map[string]string
	Labels        map[string]*Label
	LabelOrder    []string
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	NextNumber    int

	// Caches, cleared by touch() on every mutation to this repo.
	// sync.Map: reads populate them concurrently under a read lock.
	issueCache sync.Map // int -> []byte
	fetchCache sync.Map // string -> []byte
	lsCache    sync.Map // string -> []byte
	adCache    sync.Map // string -> []byte
}

// touch clears the repo's response caches. Call on every mutation.
func (r *Repo) touch() {
	r.issueCache.Clear()
	r.fetchCache.Clear()
	r.lsCache.Clear()
	r.adCache.Clear()
}

// serverIdentity is the deterministic identity every server-created commit carries.
func serverIdentity() string {
	return viewer.Name + " <" + viewer.Email + ">"
}

var isoCache sync.Map // int -> string

// seqToISO is a deterministic ISO timestamp from a sequence number (never the wall clock).
func seqToISO(seq int) string {
	if cached, ok := isoCache.Load(seq); ok {
		return cached.(string)
	}
	cached := time.Unix(scenarioEpochSec+int64(seq), 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	isoCache.Store(seq, cached)
	return cached
}

// seqToEpoch is a deterministic git commit timestamp (unix seconds) from a sequence number.
func seqToEpoch(seq int) int64 {
	return 1700000000 + int64(seq)
}

// Forge is the whole store: every repository, plus the deterministic counters.
type Forge struct {
	Repos         map[string]*Repo
	ReposByID     map[int]*Repo
	nextRepoID    int
	nextEntityID  int
	nextCommentID int
	seq           int
}

// NewForge creates an empty forge.
func NewForge() *Forge {
	return &Forge{Repos: map[string]*Repo{}, ReposByID: map[int]*Repo{}, nextRepoID: 1, nextEntityID: 1, nextCommentID: 1}
}

// NextSeq returns the next deterministic sequence number (timestamps, ordering).
func (f *Forge) NextSeq() int {
	f.seq++
	return f.seq
}

func repoKey(owner, name string) string { return owner + "/" + name }

// GetRepo returns the repo, or nil.
func (f *Forge) GetRepo(owner, name string) *Repo { return f.Repos[repoKey(owner, name)] }

// GetRepoByID returns the repo by id, or nil.
func (f *Forge) GetRepoByID(id int) *Repo { return f.ReposByID[id] }

// ListRepos returns the owner's repos in creation order.
func (f *Forge) ListRepos(owner string) []*Repo {
	var out []*Repo
	for _, repo := range f.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// CreateRepo creates a repository.
func (f *Forge) CreateRepo(owner, name string, private bool, description, defaultBranch string) *Repo {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	repo := &Repo{
		ID:            f.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranch,
		Seq:           f.NextSeq(),
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		NextNumber:    1,
	}
	f.nextRepoID++
	f.Repos[repoKey(owner, name)] = repo
	f.ReposByID[repo.ID] = repo
	return repo
}

// DeleteRepo deletes a repository.
func (f *Forge) DeleteRepo(owner, name string) bool {
	repo := f.Repos[repoKey(owner, name)]
	if repo == nil {
		return false
	}
	delete(f.Repos, repoKey(owner, name))
	delete(f.ReposByID, repo.ID)
	return true
}

// PutObject stores an object (idempotent); returns its oid.
func (f *Forge) PutObject(repo *Repo, typ gitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data, Oid: oid}
	}
	repo.touch()
	return oid
}

// Lookup returns the repo's object lookup.
func (f *Forge) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

// CreateCommit creates a server commit (contents API, merge) with a deterministic timestamp.
func (f *Forge) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	seq := f.NextSeq()
	identity := serverIdentity()
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	data := buildCommit(tree, parents, message, identity, identity, seqToEpoch(seq), "+0000")
	return f.PutObject(repo, objCommit, data)
}

// CreateIssue creates an issue.
func (f *Forge) CreateIssue(repo *Repo, title, body string, labels []*Label) *Issue {
	seq := f.NextSeq()
	issue := &Issue{
		ID:         f.nextEntityID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]*Label(nil), labels...),
		Seq:        seq,
		UpdatedSeq: seq,
	}
	f.nextEntityID++
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.touch()
	return issue
}

// CreatePull creates a pull request.
func (f *Forge) CreatePull(repo *Repo, title, body, headRef, baseRef, headSha, baseSha string) *Pull {
	seq := f.NextSeq()
	pull := &Pull{
		ID:         f.nextEntityID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    headRef,
		BaseRef:    baseRef,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		Seq:        seq,
		UpdatedSeq: seq,
	}
	f.nextEntityID++
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	repo.touch()
	return pull
}

// CreateComment adds a comment to an issue.
func (f *Forge) CreateComment(repo *Repo, issue *Issue, body string) *Comment {
	comment := &Comment{ID: f.nextCommentID, Body: body, Seq: f.NextSeq()}
	f.nextCommentID++
	issue.Comments = append(issue.Comments, comment)
	repo.touch()
	return comment
}

// ------------------------------------------------------------ JSON views
//
// Hand-rolled (no encoding/json) for speed on the hot paths. Field order
// matches the TypeScript original.

func appendUserJSON(dst []byte) []byte {
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "login")
	dst = appendJSONString(dst, viewer.Login)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, viewer.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "type")
	dst = appendJSONString(dst, "User")
	return append(dst, '}')
}

// RepoJSON renders a repo as JSON bytes.
func (f *Forge) RepoJSON(repo *Repo) []byte {
	dst := make([]byte, 0, 256)
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, repo.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "node_id")
	dst = appendJSONString(dst, globalID("Repository", itoa(repo.ID)))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "name")
	dst = appendJSONString(dst, repo.Name)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "full_name")
	dst = appendJSONString(dst, repo.Owner+"/"+repo.Name)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "owner")
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "login")
	dst = appendJSONString(dst, repo.Owner)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, viewer.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "type")
	dst = appendJSONString(dst, "User")
	dst = append(dst, '}', ',')
	dst = appendJSONKey(dst, "private")
	dst = appendJSONBool(dst, repo.Private)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "description")
	dst = appendJSONString(dst, repo.Description)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "default_branch")
	dst = appendJSONString(dst, repo.DefaultBranch)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "has_issues")
	dst = appendJSONBool(dst, true)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "created_at")
	dst = appendJSONString(dst, seqToISO(repo.Seq))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "updated_at")
	dst = appendJSONString(dst, seqToISO(repo.Seq))
	return append(dst, '}')
}

// IssueJSON renders an issue as JSON bytes. The result is cached per repo:
// the read workload GETs the same issue 2,000 times with no writes between.
func (f *Forge) IssueJSON(repo *Repo, issue *Issue) []byte {
	if cached, ok := repo.issueCache.Load(issue.Number); ok {
		return cached.([]byte)
	}
	dst := make([]byte, 0, 320)
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, issue.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "node_id")
	dst = appendJSONString(dst, globalID("Issue", itoa(repo.ID)+":"+itoa(issue.Number)))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "number")
	dst = appendJSONInt(dst, issue.Number)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "title")
	dst = appendJSONString(dst, issue.Title)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "body")
	dst = appendJSONString(dst, issue.Body)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "state")
	dst = appendJSONString(dst, issue.State)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "labels")
	dst = append(dst, '[')
	for i, label := range issue.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendJSONKey(dst, "name")
		dst = appendJSONString(dst, label.Name)
		dst = append(dst, ',')
		dst = appendJSONKey(dst, "color")
		dst = appendJSONString(dst, label.Color)
		dst = append(dst, '}')
	}
	dst = append(dst, ']', ',')
	dst = appendJSONKey(dst, "user")
	dst = appendUserJSON(dst)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "comments")
	dst = appendJSONInt(dst, len(issue.Comments))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "created_at")
	dst = appendJSONString(dst, seqToISO(issue.Seq))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "updated_at")
	dst = appendJSONString(dst, seqToISO(issue.UpdatedSeq))
	dst = append(dst, '}')
	repo.issueCache.Store(issue.Number, dst)
	return dst
}

// CommentJSON renders a comment as JSON bytes.
func (f *Forge) CommentJSON(comment *Comment) []byte {
	dst := make([]byte, 0, 192)
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, comment.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "body")
	dst = appendJSONString(dst, comment.Body)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "user")
	dst = appendUserJSON(dst)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "created_at")
	dst = appendJSONString(dst, seqToISO(comment.Seq))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "updated_at")
	dst = appendJSONString(dst, seqToISO(comment.Seq))
	return append(dst, '}')
}

// PullShas returns the pull's head/base shas, tracking the live branch tips
// (falling back to the creation shas).
func (f *Forge) PullShas(repo *Repo, pull *Pull) (headSha, baseSha string) {
	headSha = pull.HeadSha
	if sha, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headSha = sha
	}
	baseSha = pull.BaseSha
	if sha, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseSha = sha
	}
	return headSha, baseSha
}

// PullJSON renders a pull request as JSON bytes.
func (f *Forge) PullJSON(repo *Repo, pull *Pull) []byte {
	headSha, baseSha := f.PullShas(repo, pull)
	dst := make([]byte, 0, 448)
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "id")
	dst = appendJSONInt(dst, pull.ID)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "node_id")
	dst = appendJSONString(dst, globalID("PullRequest", itoa(repo.ID)+":"+itoa(pull.Number)))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "number")
	dst = appendJSONInt(dst, pull.Number)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "title")
	dst = appendJSONString(dst, pull.Title)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "body")
	dst = appendJSONString(dst, pull.Body)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "state")
	dst = appendJSONString(dst, pull.State)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "merged")
	dst = appendJSONBool(dst, pull.Merged)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "merged_at")
	if pull.HasMergedAt {
		dst = appendJSONString(dst, pull.MergedAt)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "merge_commit_sha")
	if pull.HasMergeSha {
		dst = appendJSONString(dst, pull.MergeCommitSha)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "head")
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "ref")
	dst = appendJSONString(dst, pull.HeadRef)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "sha")
	dst = appendJSONString(dst, headSha)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "label")
	dst = appendJSONString(dst, repo.Owner+":"+pull.HeadRef)
	dst = append(dst, '}', ',')
	dst = appendJSONKey(dst, "base")
	dst = append(dst, '{')
	dst = appendJSONKey(dst, "ref")
	dst = appendJSONString(dst, pull.BaseRef)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "sha")
	dst = appendJSONString(dst, baseSha)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "label")
	dst = appendJSONString(dst, repo.Owner+":"+pull.BaseRef)
	dst = append(dst, '}', ',')
	dst = appendJSONKey(dst, "user")
	dst = appendUserJSON(dst)
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "created_at")
	dst = appendJSONString(dst, seqToISO(pull.Seq))
	dst = append(dst, ',')
	dst = appendJSONKey(dst, "updated_at")
	dst = appendJSONString(dst, seqToISO(pull.UpdatedSeq))
	return append(dst, '}')
}

// repoStore adapts a repo to the storeObjects interface.
type repoStore struct {
	forge *Forge
	repo  *Repo
}

func (s repoStore) put(typ gitObjectType, data []byte) string {
	return s.forge.PutObject(s.repo, typ, data)
}

// MergePull merges a pull request. method is "merge" (a two-parent merge
// commit) or "squash" (one parent); anything else is treated as squash.
// Returns the merge commit sha, or "" when the pull cannot merge (with
// conflict set when the reason is a conflict).
func (f *Forge) MergePull(repo *Repo, pull *Pull, method string) (sha string, conflict bool) {
	if pull.State != "open" || pull.Merged {
		return "", false
	}
	lookup := f.Lookup(repo)
	headTip, ok1 := repo.Refs["refs/heads/"+pull.HeadRef]
	baseTip, ok2 := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok1 || !ok2 {
		return "", false
	}
	headCommit := lookup(headTip)
	baseCommit := lookup(baseTip)
	if headCommit == nil || baseCommit == nil {
		return "", false
	}
	base := mergeBase(lookup, baseTip, headTip)
	baseTree := ""
	if base != "" {
		if o := lookup(base); o != nil {
			baseTree = parseCommit(o.Data).Tree
		}
	}
	outcome := threeWayMerge(lookup, repoStore{f, repo}, parseCommit(baseCommit.Data).Tree, parseCommit(headCommit.Data).Tree, baseTree)
	if outcome.Tree == "" {
		return "", true
	}
	isMerge := method == "merge"
	var message string
	if isMerge {
		message = fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s\n", pull.Number, repo.Owner, pull.HeadRef, pull.Title)
	} else {
		message = fmt.Sprintf("%s (#%d)\n", pull.Title, pull.Number)
	}
	var parents []string
	if isMerge {
		parents = []string{baseTip, headTip}
	} else {
		parents = []string{baseTip}
	}
	sha = f.CreateCommit(repo, outcome.Tree, parents, message)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = seqToISO(f.NextSeq())
	pull.HasMergedAt = true
	pull.MergeCommitSha = sha
	pull.HasMergeSha = true
	pull.UpdatedSeq = f.NextSeq()
	repo.touch()
	return sha, false
}

// PullMergeable reports whether a pull request would merge cleanly right now
// (MERGEABLE vs CONFLICTING vs UNKNOWN).
func (f *Forge) PullMergeable(repo *Repo, pull *Pull) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	lookup := f.Lookup(repo)
	headTip, ok1 := repo.Refs["refs/heads/"+pull.HeadRef]
	baseTip, ok2 := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok1 || !ok2 {
		return "UNKNOWN"
	}
	headCommit := lookup(headTip)
	baseCommit := lookup(baseTip)
	if headCommit == nil || baseCommit == nil {
		return "UNKNOWN"
	}
	base := mergeBase(lookup, baseTip, headTip)
	baseTree := ""
	if base != "" {
		if o := lookup(base); o != nil {
			baseTree = parseCommit(o.Data).Tree
		}
	}
	// A dry-run merge into the real sink: the tree objects it builds are harmless (idempotent).
	outcome := threeWayMerge(lookup, repoStore{f, repo}, parseCommit(baseCommit.Data).Tree, parseCommit(headCommit.Data).Tree, baseTree)
	if outcome.Tree == "" {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}
