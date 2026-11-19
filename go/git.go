// Git smart-HTTP: the wire protocol a real `git` client speaks, implemented
// from the packfile up. No git binary, no shelling out — every byte is framed here.
//
//   info/refs   - v0 advertisements (`# service=...` + ref lines, capabilities after a NUL on the
//                 first line) and the v2 advertisement (`version 2` + capability lines), chosen by
//                 the client's `Git-Protocol: version=2` header. receive-pack is v0 only.
//   upload-pack - protocol v2 `ls-refs` and `fetch` (plus a minimal v0 fetch for completeness).
//   receive-pack - v0 command list + pack, with report-status inside sideband band 1.
//
// pkt-line framing: a 4-hex-digit length (itself included) + payload; 0000 = flush, 0001 = delim,
// 0002 = response-end. Sideband packets carry a band byte (1 = data, 2 = progress, 3 = error)
// ahead of their payload. The report-status stream inside band 1 is itself pkt-line framed.
//
// Performance: the v2 advertisement is a constant; v0 advertisements, ls-refs
// answers and whole fetch responses are cached per repo and cleared on every
// mutation (Repo.touch), so the benchmark's 50 identical concurrent clones
// build their pack exactly once.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GitHTTPError is an HTTP-level failure (malformed framing, unknown command):
// the caller renders it as JSON.
type GitHTTPError struct {
	Status  int
	Message string
}

func (e *GitHTTPError) Error() string { return e.Message }

// GitHTTPResponse is one git endpoint's answer.
type GitHTTPResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

type packetKind int

const (
	pktData packetKind = iota
	pktFlushKind
	pktDelimKind
)

type packet struct {
	kind    packetKind
	payload []byte
}

// ---------------------------------------------------------------- pkt-line codec

const maxPktLine = 65520

// pktText is a pkt-line carrying text (the caller supplies the trailing newline when one belongs there).
func pktText(text string) []byte {
	return pktRaw([]byte(text))
}

// pktRaw is a pkt-line carrying raw bytes.
func pktRaw(payload []byte) []byte {
	length := len(payload) + 4
	if length > maxPktLine {
		panic(&GitHTTPError{Status: 500, Message: fmt.Sprintf("pkt-line of %d bytes exceeds the 65520 limit", length)})
	}
	out := make([]byte, 0, length)
	out = append(out, hex4(length)...)
	out = append(out, payload...)
	return out
}

func hex4(n int) string {
	const hexd = "0123456789abcdef"
	return string([]byte{hexd[(n>>12)&0xf], hexd[(n>>8)&0xf], hexd[(n>>4)&0xf], hexd[n&0xf]})
}

var pktFlushBytes = []byte("0000")
var pktDelimBytes = []byte("0001")

// pktSideband is a sideband packet: a pkt-line whose payload starts with the band byte, then the data.
func pktSideband(band byte, data []byte) []byte {
	payload := make([]byte, 0, len(data)+1)
	payload = append(payload, band)
	payload = append(payload, data...)
	return pktRaw(payload)
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// readPacket reads one packet at offset; returns the packet and the offset just past it.
func readPacket(buf []byte, offset int) (packet, int, error) {
	if offset+4 > len(buf) {
		return packet{}, 0, &GitHTTPError{Status: 400, Message: fmt.Sprintf("pkt-line header at %d is truncated", offset)}
	}
	header := string(buf[offset : offset+4])
	if !isHex4(header) {
		return packet{}, 0, &GitHTTPError{Status: 400, Message: fmt.Sprintf("pkt-line at %d starts with %q, not a length", offset, header)}
	}
	length, _ := strconv.ParseInt(header, 16, 32)
	switch {
	case length == 0:
		return packet{kind: pktFlushKind}, offset + 4, nil
	case length == 1:
		return packet{kind: pktDelimKind}, offset + 4, nil
	case length == 2:
		// response-end: a terminator, like flush.
		return packet{kind: pktFlushKind}, offset + 4, nil
	case length == 3:
		return packet{}, 0, &GitHTTPError{Status: 400, Message: fmt.Sprintf("pkt-line at %d has the invalid length 3", offset)}
	case length > maxPktLine:
		return packet{}, 0, &GitHTTPError{Status: 400, Message: fmt.Sprintf("pkt-line at %d claims %d bytes, over the 65520 limit", offset, length)}
	case offset+int(length) > len(buf):
		return packet{}, 0, &GitHTTPError{Status: 400, Message: fmt.Sprintf("pkt-line at %d claims %d bytes but only %d remain", offset, length, len(buf)-offset)}
	}
	return packet{kind: pktData, payload: buf[offset+4 : offset+int(length)]}, offset + int(length), nil
}

// parseUntilFlush parses packets until the first flush (inclusive). consumed
// is the offset just past the flush, so the caller can find what follows it
// (a packfile, for receive-pack).
func parseUntilFlush(buf []byte) (packets []packet, consumed int, foundFlush bool, err error) {
	offset := 0
	for offset < len(buf) {
		var p packet
		var next int
		p, next, err = readPacket(buf, offset)
		if err != nil {
			return nil, 0, false, err
		}
		offset = next
		if p.kind == pktFlushKind {
			return packets, offset, true, nil
		}
		packets = append(packets, p)
	}
	return packets, offset, false, nil
}

// parseAllPackets parses every packet in the buffer (a flush does not stop the scan).
func parseAllPackets(buf []byte) ([]packet, error) {
	var packets []packet
	offset := 0
	for offset < len(buf) {
		p, next, err := readPacket(buf, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		packets = append(packets, p)
	}
	return packets, nil
}

// ---------------------------------------------------------------- refs

type refPair struct {
	name string
	oid  string
}

// sortedRefs returns every ref in the repo, sorted by name (branches sort before tags).
func sortedRefs(repo *Repo) []refPair {
	refs := make([]refPair, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, refPair{name: name, oid: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

// peelToCommit peels an annotated tag to the commit it ultimately points to,
// or "" when the ref is not an annotated tag that resolves to a commit.
func peelToCommit(repo *Repo, oid string) string {
	current := oid
	seen := make(map[string]bool)
	for hops := 0; hops < 10; hops++ {
		object := repo.Objects[current]
		if object == nil {
			return ""
		}
		if object.Type == objCommit {
			if hops == 0 {
				return ""
			}
			return current
		}
		if object.Type != objTag {
			return ""
		}
		if seen[current] {
			return ""
		}
		seen[current] = true
		current = parseTag(object.Data).Object
	}
	return ""
}

func headRefName(repo *Repo) string { return "refs/heads/" + repo.DefaultBranch }

// ---------------------------------------------------------------- advertisements

func uploadPackV0Capabilities(repo *Repo) string {
	return "side-band-64k symref=HEAD:" + headRefName(repo) + " ofs-delta thin-pack multi_ack shallow"
}

const receivePackCapabilities = "report-status side-band-64k delete-refs ofs-delta"

// v0Advertisement is a v0 advertisement: `# service=...`, flush, ref lines
// (capabilities after a NUL on the first), flush. Cached per repo; cleared on mutation.
func v0Advertisement(repo *Repo, capabilities, service string) []byte {
	if repo.adCache != nil {
		if cached, ok := repo.adCache[service]; ok {
			return cached
		}
	}
	var out []byte
	out = append(out, pktText("# service="+service+"\n")...)
	out = append(out, pktFlushBytes...)
	refs := sortedRefs(repo)
	headOid := repo.Refs[headRefName(repo)]
	if len(refs) == 0 {
		out = append(out, pktText(zeroOID+" capabilities^{}\x00"+capabilities+"\n")...)
	} else {
		if headOid == "" {
			headOid = zeroOID
		}
		out = append(out, pktText(headOid+" HEAD\x00"+capabilities+"\n")...)
		for _, ref := range refs {
			out = append(out, pktText(ref.oid+" "+ref.name+"\n")...)
			if strings.HasPrefix(ref.name, "refs/tags/") {
				if peeled := peelToCommit(repo, ref.oid); peeled != "" {
					out = append(out, pktText(peeled+" "+ref.name+"^{}\n")...)
				}
			}
		}
	}
	out = append(out, pktFlushBytes...)
	if repo.adCache == nil {
		repo.adCache = map[string][]byte{}
	}
	repo.adCache[service] = out
	return out
}

// v2AdvertisementBytes is the v2 advertisement: `version 2`, then one
// capability line each. It carries no repo state, so it is a constant.
var v2AdvertisementBytes = func() []byte {
	var out []byte
	out = append(out, pktText("version 2\n")...)
	out = append(out, pktText("fetch=shallow thin-pack ofs-delta\n")...)
	out = append(out, pktText("ls-refs\n")...)
	out = append(out, pktText("object-format=sha1\n")...)
	out = append(out, pktFlushBytes...)
	return out
}()

// handleInfoRefs serves GET `/{owner}/{repo}.git/info/refs?service=...`.
func handleInfoRefs(repo *Repo, service, gitProtocol string) (GitHTTPResponse, error) {
	if service == "git-upload-pack" {
		if gitProtocol == "version=2" {
			return GitHTTPResponse{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v2AdvertisementBytes}, nil
		}
		return GitHTTPResponse{Status: 200, ContentType: "application/x-git-upload-pack-advertisement", Body: v0Advertisement(repo, uploadPackV0Capabilities(repo), "git-upload-pack")}, nil
	}
	if service == "git-receive-pack" {
		// receive-pack is v0 only, whatever the client asked for.
		return GitHTTPResponse{Status: 200, ContentType: "application/x-git-receive-pack-advertisement", Body: v0Advertisement(repo, receivePackCapabilities, "git-receive-pack")}, nil
	}
	return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: fmt.Sprintf("unknown git service %q", service)}
}

// ---------------------------------------------------------------- ls-refs (v2)

// handleLsRefs serves POST git-upload-pack, `command=ls-refs`: the ref list,
// honouring peel/symrefs/ref-prefix. Cached per repo + args.
func handleLsRefs(repo *Repo, args []string) []byte {
	key := strings.Join(args, "\n")
	if repo.lsCache != nil {
		if cached, ok := repo.lsCache[key]; ok {
			return cached
		}
	}
	peel := false
	symrefs := false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	headTarget := headRefName(repo)
	headOid := repo.Refs[headTarget]
	if headOid == "" {
		headOid = zeroOID
	}
	if keep("HEAD") {
		line := headOid + " HEAD"
		if symrefs {
			line += " symref-target:" + headTarget
		}
		out = append(out, pktText(line+"\n")...)
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if peel && strings.HasPrefix(ref.name, "refs/tags/") {
			if peeled := peelToCommit(repo, ref.oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		out = append(out, pktText(line+"\n")...)
	}
	out = append(out, pktFlushBytes...)
	if repo.lsCache == nil {
		repo.lsCache = map[string][]byte{}
	}
	repo.lsCache[key] = out
	return out
}

// ---------------------------------------------------------------- fetch

type fetchArgs struct {
	wants      []string
	haves      []string // insertion order, deduplicated
	haveSet    map[string]bool
	shallows   []string // insertion order, deduplicated
	shallowSet map[string]bool
	depth      int
	hasDepth   bool
}

func parseFetchArgs(args []string) fetchArgs {
	var out fetchArgs
	out.haveSet = map[string]bool{}
	out.shallowSet = map[string]bool{}
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			oid := strings.TrimSpace(arg[5:])
			if isObjectID(oid) {
				out.wants = append(out.wants, oid)
			}
		case strings.HasPrefix(arg, "have "):
			oid := strings.TrimSpace(arg[5:])
			if isObjectID(oid) && !out.haveSet[oid] {
				out.haveSet[oid] = true
				out.haves = append(out.haves, oid)
			}
		case strings.HasPrefix(arg, "shallow "):
			oid := strings.TrimSpace(arg[8:])
			if isObjectID(oid) && !out.shallowSet[oid] {
				out.shallowSet[oid] = true
				out.shallows = append(out.shallows, oid)
			}
		case strings.HasPrefix(arg, "deepen "):
			value, err := strconv.Atoi(strings.TrimSpace(arg[7:]))
			if err == nil && value > 0 {
				out.depth = value
				out.hasDepth = true
			}
		}
		// thin-pack, no-progress, ofs-delta, done, filter, deepen-since, deepen-not: acknowledged, ignored.
	}
	return out
}

type fetchPlan struct {
	objects     []*GitObject
	shallow     []string
	unshallow   []string
	commonHaves []string
}

// planFetch resolves a fetch request against the store: which objects to send, which boundaries move.
func planFetch(forge *Forge, repo *Repo, args fetchArgs) fetchPlan {
	lookup := forge.Lookup(repo)
	reach := collectReachable(lookup, args.wants, args.depth, args.hasDepth, args.haveSet, args.shallowSet)
	newShallowSet := make(map[string]bool, len(reach.Shallow))
	for _, s := range reach.Shallow {
		newShallowSet[s] = true
	}
	var unshallow []string
	for _, boundary := range args.shallows {
		if !reach.Visited[boundary] {
			continue
		}
		if newShallowSet[boundary] {
			continue
		}
		object := lookup(boundary)
		if object == nil || object.Type != objCommit {
			continue
		}
		parents := parseCommit(object.Data).Parents
		complete := true
		for _, parent := range parents {
			if !(args.haveSet[parent] || reach.Objects[parent] || lookup(parent) == nil) {
				complete = false
				break
			}
		}
		if complete {
			unshallow = append(unshallow, boundary)
		}
	}
	var commonHaves []string
	for _, have := range args.haves {
		if lookup(have) == nil {
			continue
		}
		for _, want := range args.wants {
			if isAncestor(lookup, have, want) {
				commonHaves = append(commonHaves, have)
				break
			}
		}
	}
	packObjects := make([]*GitObject, 0, len(reach.Objects))
	for oid := range reach.Objects {
		if object := lookup(oid); object != nil {
			packObjects = append(packObjects, object)
		}
	}
	return fetchPlan{objects: packObjects, shallow: reach.Shallow, unshallow: unshallow, commonHaves: commonHaves}
}

// packSideband renders a packfile as sideband band-1 packets (chunks stay well inside the pkt-line limit).
func packSideband(objects []*GitObject) [][]byte {
	pack := writePack(objects)
	var parts [][]byte
	const chunk = 65000
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		parts = append(parts, pktSideband(1, pack[offset:end]))
	}
	return parts
}

// handleFetchV2 serves POST git-upload-pack, `command=fetch` (v2). The whole
// response is cached per repo + canonical args: the benchmark's 50 clones
// share one pack build.
func handleFetchV2(forge *Forge, repo *Repo, args []string) []byte {
	parsed := parseFetchArgs(args)
	wantsDepth := false
	hasShallows := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "deepen ") {
			wantsDepth = true
		}
		if strings.HasPrefix(arg, "shallow ") {
			hasShallows = true
		}
	}
	var keyBuf bytes.Buffer
	keyBuf.WriteString(strings.Join(parsed.wants, ","))
	keyBuf.WriteByte('|')
	keyBuf.WriteString(strings.Join(parsed.haves, ","))
	keyBuf.WriteByte('|')
	keyBuf.WriteString(strings.Join(parsed.shallows, ","))
	keyBuf.WriteByte('|')
	if parsed.hasDepth {
		keyBuf.WriteString(itoa(parsed.depth))
	}
	keyBuf.WriteByte('|')
	if wantsDepth {
		keyBuf.WriteByte('1')
	} else {
		keyBuf.WriteByte('0')
	}
	if hasShallows {
		keyBuf.WriteByte('1')
	} else {
		keyBuf.WriteByte('0')
	}
	key := keyBuf.String()
	if repo.fetchCache != nil {
		if cached, ok := repo.fetchCache[key]; ok {
			return cached
		}
	}
	plan := planFetch(forge, repo, parsed)
	var out []byte
	out = append(out, pktText("acknowledgments\n")...)
	if len(plan.commonHaves) == 0 {
		out = append(out, pktText("NAK\n")...)
	} else {
		for _, have := range plan.commonHaves {
			out = append(out, pktText("ACK "+have+"\n")...)
		}
		out = append(out, pktText("ready\n")...)
	}
	out = append(out, pktDelimBytes...)
	if wantsDepth || hasShallows {
		out = append(out, pktText("shallow-info\n")...)
		for _, oid := range plan.shallow {
			out = append(out, pktText("shallow "+oid+"\n")...)
		}
		for _, oid := range plan.unshallow {
			out = append(out, pktText("unshallow "+oid+"\n")...)
		}
		out = append(out, pktDelimBytes...)
	}
	if len(plan.objects) > 0 {
		out = append(out, pktText("packfile\n")...)
		for _, part := range packSideband(plan.objects) {
			out = append(out, part...)
		}
	}
	out = append(out, pktFlushBytes...)
	if repo.fetchCache == nil {
		repo.fetchCache = map[string][]byte{}
	}
	repo.fetchCache[key] = out
	return out
}

// handleFetchV0 serves a v0 fetch: want lines, flush, have/done lines, flush.
func handleFetchV0(forge *Forge, repo *Repo, body []byte) ([]byte, error) {
	packets, err := parseAllPackets(body)
	if err != nil {
		return nil, err
	}
	var args []string
	for _, p := range packets {
		if p.kind == pktData {
			args = append(args, strings.TrimRight(string(p.payload), " \t\n\r"))
		}
	}
	plan := planFetch(forge, repo, parseFetchArgs(args))
	var out []byte
	if len(plan.commonHaves) == 0 {
		out = append(out, pktText("NAK\n")...)
	} else {
		for _, have := range plan.commonHaves {
			out = append(out, pktText("ACK "+have+"\n")...)
		}
	}
	for _, oid := range plan.shallow {
		out = append(out, pktText("shallow "+oid+"\n")...)
	}
	for _, oid := range plan.unshallow {
		out = append(out, pktText("unshallow "+oid+"\n")...)
	}
	if len(plan.objects) > 0 {
		for _, part := range packSideband(plan.objects) {
			out = append(out, part...)
		}
	}
	out = append(out, pktFlushBytes...)
	return out, nil
}

// handleUploadPack serves POST `/{owner}/{repo}.git/git-upload-pack`.
// v2 when the client asked for it, v0 otherwise.
func handleUploadPack(forge *Forge, repo *Repo, body []byte, gitProtocol string) (GitHTTPResponse, error) {
	if gitProtocol != "version=2" {
		out, err := handleFetchV0(forge, repo, body)
		if err != nil {
			return GitHTTPResponse{}, err
		}
		return GitHTTPResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out}, nil
	}
	packets, err := parseAllPackets(body)
	if err != nil {
		return GitHTTPResponse{}, err
	}
	var first *packet
	for i := range packets {
		if packets[i].kind == pktData {
			first = &packets[i]
			break
		}
	}
	if first == nil {
		return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: "upload-pack v2 request has no command line"}
	}
	commandLine := strings.TrimRight(string(first.payload), " \t\n\r")
	if !strings.HasPrefix(commandLine, "command=") {
		head := commandLine
		if len(head) > 40 {
			head = head[:40]
		}
		return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: fmt.Sprintf("upload-pack v2 request starts with %q, not a command", head)}
	}
	command := commandLine[len("command="):]
	delimAt := -1
	for i := range packets {
		if packets[i].kind == pktDelimKind {
			delimAt = i
			break
		}
	}
	var argPackets []packet
	if delimAt < 0 {
		argPackets = packets[1:]
	} else {
		argPackets = packets[delimAt+1:]
	}
	var args []string
	for _, p := range argPackets {
		if p.kind == pktData {
			args = append(args, strings.TrimRight(string(p.payload), " \t\n\r"))
		}
	}
	switch command {
	case "ls-refs":
		return GitHTTPResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: handleLsRefs(repo, args)}, nil
	case "fetch":
		return GitHTTPResponse{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: handleFetchV2(forge, repo, args)}, nil
	default:
		return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: fmt.Sprintf("unknown upload-pack command %q", command)}
	}
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

type pushResult struct {
	ref    string
	ok     bool
	reason string
}

// handleReceivePack serves POST `/{owner}/{repo}.git/git-receive-pack` (v0 only).
func handleReceivePack(forge *Forge, repo *Repo, body []byte) (GitHTTPResponse, error) {
	packets, consumed, foundFlush, err := parseUntilFlush(body)
	if err != nil {
		return GitHTTPResponse{}, err
	}
	if !foundFlush {
		return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: "receive-pack request has no flush after its command list"}
	}
	var commands []pushCommand
	var clientCapabilities []string
	for index, p := range packets {
		if p.kind != pktData {
			continue
		}
		text := string(p.payload)
		nulAt := strings.Index(text, "\x00")
		line := text
		if nulAt >= 0 {
			line = text[:nulAt]
		}
		line = strings.TrimRight(line, " \t\n\r\v\f")
		if index == 0 && nulAt >= 0 {
			clientCapabilities = strings.Fields(strings.TrimSpace(text[nulAt+1:]))
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 || !isObjectID(parts[0]) || !isObjectID(parts[1]) || parts[2] == "" {
			head := line
			if len(head) > 80 {
				head = head[:80]
			}
			return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: fmt.Sprintf("malformed receive-pack command %q", head)}
		}
		commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
	}
	if len(commands) == 0 {
		return GitHTTPResponse{}, &GitHTTPError{Status: 400, Message: "receive-pack request has no commands"}
	}

	lookup := forge.Lookup(repo)
	packBytes := body[consumed:]
	unpackError := ""
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, lookup)
		if err != nil {
			unpackError = err.Error()
		} else {
			for oid, object := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = object
				}
			}
		}
	}

	var results []pushResult
	if unpackError != "" {
		for _, command := range commands {
			results = append(results, pushResult{ref: command.ref, ok: false, reason: "unpack failed"})
		}
	} else {
		for _, command := range commands {
			current, hasCurrent := repo.Refs[command.ref]
			if !strings.HasPrefix(command.ref, "refs/") {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "invalid ref name"})
				continue
			}
			if command.newOid == zeroOID {
				// Delete: idempotent, always accepted.
				delete(repo.Refs, command.ref)
				results = append(results, pushResult{ref: command.ref, ok: true})
				continue
			}
			target := lookup(command.newOid)
			if target == nil {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "object not found"})
				continue
			}
			if strings.HasPrefix(command.ref, "refs/heads/") && target.Type != objCommit {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "not a commit"})
				continue
			}
			if command.oldOid == zeroOID {
				// Create.
				if hasCurrent {
					results = append(results, pushResult{ref: command.ref, ok: false, reason: "already exists"})
					continue
				}
				repo.Refs[command.ref] = command.newOid
				results = append(results, pushResult{ref: command.ref, ok: true})
				continue
			}
			// Update.
			if !hasCurrent {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "ref does not exist"})
				continue
			}
			if current != command.oldOid {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "old value does not match"})
				continue
			}
			if command.newOid == command.oldOid {
				results = append(results, pushResult{ref: command.ref, ok: true})
				continue
			}
			if strings.HasPrefix(command.ref, "refs/tags/") {
				// Tags are immutable: any real update is refused.
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "already exists"})
				continue
			}
			if !isAncestor(lookup, command.oldOid, command.newOid) {
				results = append(results, pushResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
			repo.Refs[command.ref] = command.newOid
			results = append(results, pushResult{ref: command.ref, ok: true})
		}
	}
	repo.touch()

	var reportLines []string
	if unpackError == "" {
		reportLines = append(reportLines, "unpack ok")
	} else {
		reportLines = append(reportLines, "unpack error "+unpackError)
	}
	for _, result := range results {
		if result.ok {
			reportLines = append(reportLines, "ok "+result.ref)
		} else {
			reportLines = append(reportLines, "ng "+result.ref+" "+result.reason)
		}
	}
	sideband := false
	for _, cap := range clientCapabilities {
		if cap == "side-band-64k" {
			sideband = true
			break
		}
	}
	var out []byte
	if sideband {
		// The report-status stream is itself pkt-lined inside band 1.
		for _, line := range reportLines {
			out = append(out, pktSideband(1, pktText(line+"\n"))...)
		}
	} else {
		for _, line := range reportLines {
			out = append(out, pktText(line+"\n")...)
		}
	}
	out = append(out, pktFlushBytes...)
	return GitHTTPResponse{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: out}, nil
}
