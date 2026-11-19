// The HTTP server: a hand-rolled HTTP/1.1 server over raw TCP (no net/http:
// smaller binary, fewer allocations, full control of keep-alive), plus
// routing, auth, and the REST surface, with seams to the git, GraphQL and
// MCP handlers.
//
//   - Every reply carries a non-empty `X-GitHub-Request-Id` (`req-<n>`, a deterministic counter).
//   - Every REST reply is `application/json`; errors carry a JSON body with a string `message`.
//   - Auth is `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the env's token). When the variable is
//     unset or empty — local runs — any non-empty Bearer token is accepted; a missing token is
//     always a 401. Public routes: /healthz, GET /api/v3/, GET /api/v3/meta.
//   - A missing repo (or a repo under another owner) is a 404, never a 403.
//   - A malformed JSON body is a 400; a missing or invalid field is a 422.
//   - The git endpoints speak their own content types; everything else is JSON.
//
// Concurrency: one mutex serialises request handling. The suite's checks are
// sequential; the only concurrent workload is the clone benchmark, whose
// requests are all cache hits of a few microseconds, so serialisation costs
// nothing measurable and keeps the store (and its deterministic sequence)
// trivially consistent.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxBodyBytes = 64 * 1024 * 1024
const maxHeaderBytes = 64 * 1024

// ---------------------------------------------------------------- HTTP server

// httpRequest is one parsed request.
type httpRequest struct {
	method           string
	rawURL           string
	version11        bool
	host             string
	authorization    string
	gitProtocol      string
	connection       string
	expect100        bool
	chunked          bool
	contentLength    int64
	hasContentLength bool
	body             []byte
}

// httpResponse is one response, built under the lock and written after it.
type httpResponse struct {
	status        int
	contentType   string
	body          []byte
	extra         [][2]string // extra headers, in order
	noContentType bool
}

// app is the server: the forge, the lock, auth, and the request counter.
//
// The lock is an RWMutex: read-only requests (GET/HEAD, plus POST
// git-upload-pack, which only ever reads) run concurrently under a read
// lock, while mutations take the write lock. That matters for the clone
// workload: 50 concurrent clones used to serialize on one mutex. Reads
// never mutate store state: the response caches are sync.Maps and the
// deterministic sequence only advances on mutations (write-locked), so
// concurrent reads see a consistent store and the exported state stays
// a pure function of the (sequential) mutation order.
type app struct {
	forge          *Forge
	mu             sync.RWMutex
	expectedToken  string
	requestCounter atomic.Int64
}

func newApp() *app {
	return &app{forge: NewForge(), expectedToken: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}

// serve accepts connections on ln until it closes.
func (a *app) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.handleConn(conn)
	}
}

func (a *app) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 65536)
	for {
		req, keepAlive, err := readRequest(reader, conn)
		if err != nil {
			return
		}
		resp := a.handle(req)
		if err := writeResponse(conn, req, resp, keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRequest parses one request from the connection. It returns the request,
// whether the connection stays alive after the response, and an error when
// the connection is unusable.
//
// The hot path allocates as little as possible: header lines are parsed
// in place from the reader's buffer (readLine aliases it) and only the
// values the request keeps become strings; headers the server ignores
// (accept, user-agent, ...) cost no allocation at all.
func readRequest(reader *bufio.Reader, conn net.Conn) (*httpRequest, bool, error) {
	line, err := readLine(reader)
	if err != nil {
		return nil, false, err
	}
	if len(line) == 0 {
		// A stray blank line before a request (a client probe): skip it.
		line, err = readLine(reader)
		if err != nil {
			return nil, false, err
		}
	}
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, false, errors.New("malformed request line")
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return nil, false, errors.New("malformed request line")
	}
	sp2 += sp1 + 1
	req := &httpRequest{method: string(line[:sp1]), rawURL: string(line[sp1+1 : sp2])}
	req.version11 = bytes.Equal(line[sp2+1:], http11Bytes)
	headerBytes := len(line)
	for {
		hdr, err := readLine(reader)
		if err != nil {
			return nil, false, err
		}
		headerBytes += len(hdr)
		if headerBytes > maxHeaderBytes {
			return nil, false, errors.New("headers too large")
		}
		if len(hdr) == 0 {
			break
		}
		colon := bytes.IndexByte(hdr, ':')
		if colon < 0 {
			continue
		}
		name := trimSpaceBytes(hdr[:colon])
		value := trimSpaceBytes(hdr[colon+1:])
		switch {
		case eqFoldASCII(name, hdrHost):
			req.host = string(value)
		case eqFoldASCII(name, hdrAuthorization):
			req.authorization = string(value)
		case eqFoldASCII(name, hdrGitProtocol):
			req.gitProtocol = string(value)
		case eqFoldASCII(name, hdrConnection):
			req.connection = string(value)
		case eqFoldASCII(name, hdrExpect):
			if eqFoldASCII(value, expectContinue) {
				req.expect100 = true
			}
		case eqFoldASCII(name, hdrContentLength):
			if n, ok := parseUintBytes(value); ok {
				req.contentLength = int64(n)
				req.hasContentLength = true
			}
		case eqFoldASCII(name, hdrTransferEncoding):
			if containsFoldASCII(value, chunkedBytes) {
				req.chunked = true
			}
		}
	}
	if req.host == "" {
		req.host = "127.0.0.1"
	}
	keepAlive := req.version11
	if containsFoldASCIIString(req.connection, closeBytes) {
		keepAlive = false
	} else if !req.version11 && containsFoldASCIIString(req.connection, keepAliveBytes) {
		keepAlive = true
	}
	// Read the body.
	if req.expect100 && (req.chunked || req.contentLength > 0) {
		conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	if req.chunked {
		body, err := readChunkedBody(reader)
		if err != nil {
			return nil, false, err
		}
		req.body = body
	} else if req.hasContentLength {
		if req.contentLength > maxBodyBytes {
			return nil, false, errors.New("request body too large")
		}
		if req.contentLength > 0 {
			body := make([]byte, req.contentLength)
			if _, err := io.ReadFull(reader, body); err != nil {
				return nil, false, err
			}
			req.body = body
		}
	}
	return req, keepAlive, nil
}

// Header names and tokens the request parser matches, as byte slices so
// the comparisons allocate nothing. All lowercase: matching folds ASCII.
var (
	http11Bytes         = []byte("HTTP/1.1")
	hdrHost             = []byte("host")
	hdrAuthorization    = []byte("authorization")
	hdrGitProtocol      = []byte("git-protocol")
	hdrConnection       = []byte("connection")
	hdrExpect           = []byte("expect")
	hdrContentLength    = []byte("content-length")
	hdrTransferEncoding = []byte("transfer-encoding")
	expectContinue      = []byte("100-continue")
	chunkedBytes        = []byte("chunked")
	closeBytes          = []byte("close")
	keepAliveBytes      = []byte("keep-alive")
)

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// eqFoldASCII reports whether a equals b, folding ASCII case. b must already
// be lowercase.
func eqFoldASCII(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if lowerByte(a[i]) != b[i] {
			return false
		}
	}
	return true
}

// containsFoldASCII reports whether hay contains needle, folding ASCII case.
// needle must already be lowercase.
func containsFoldASCII(hay, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if lowerByte(hay[i+j]) != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// containsFoldASCIIString is containsFoldASCII for a string haystack.
func containsFoldASCIIString(hay string, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if lowerByte(hay[i+j]) != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// trimSpaceBytes trims ASCII spaces and tabs from both ends.
func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// parseUintBytes parses a non-negative decimal integer.
func parseUintBytes(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

// readLine reads one CRLF- or LF-terminated line, without the terminator.
// The returned slice aliases the reader's buffer: it is valid only until
// the next read from the reader. Copy anything that must outlive it.
func readLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// Over-long line: accumulate fragments (rare; headers are short).
		buf := append([]byte(nil), line...)
		for err == bufio.ErrBufferFull {
			var frag []byte
			frag, err = reader.ReadSlice('\n')
			buf = append(buf, frag...)
		}
		if err != nil {
			return nil, err
		}
		line = buf
	} else if err != nil {
		return nil, err
	}
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
	}
	return line, nil
}

// readChunkedBody decodes a chunked request body.
func readChunkedBody(reader *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := readLine(reader)
		if err != nil {
			return nil, err
		}
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, ok := parseHexBytes(trimSpaceBytes(line))
		if !ok {
			return nil, errors.New("malformed chunk size")
		}
		if size == 0 {
			// Trailers until a blank line.
			for {
				trailer, err := readLine(reader)
				if err != nil {
					return nil, err
				}
				if len(trailer) == 0 {
					break
				}
			}
			return body, nil
		}
		if int64(len(body))+int64(size) > maxBodyBytes {
			return nil, errors.New("request body too large")
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		if _, err := readLine(reader); err != nil {
			return nil, err
		}
	}
}

// parseHexBytes parses a non-negative hexadecimal integer.
func parseHexBytes(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		var v uint64
		switch {
		case c >= '0' && c <= '9':
			v = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			v = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = uint64(c-'A') + 10
		default:
			return 0, false
		}
		n = n*16 + v
	}
	return n, true
}

var statusTexts = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// writeResponse writes one response in a single Write call where possible.
func writeResponse(conn net.Conn, req *httpRequest, resp httpResponse, keepAlive bool) error {
	text, ok := statusTexts[resp.status]
	if !ok {
		text = "Status"
	}
	var out []byte
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(resp.status), 10)
	out = append(out, ' ')
	out = append(out, text...)
	out = append(out, "\r\n"...)
	if !resp.noContentType {
		ct := resp.contentType
		if ct == "" {
			ct = "application/json"
		}
		out = append(out, "content-type: "...)
		out = append(out, ct...)
		out = append(out, "\r\n"...)
	}
	out = append(out, "content-length: "...)
	out = strconv.AppendInt(out, int64(len(resp.body)), 10)
	out = append(out, "\r\n"...)
	for _, h := range resp.extra {
		out = append(out, h[0]...)
		out = append(out, ": "...)
		out = append(out, h[1]...)
		out = append(out, "\r\n"...)
	}
	if keepAlive {
		out = append(out, "connection: keep-alive\r\n"...)
	} else {
		out = append(out, "connection: close\r\n"...)
	}
	out = append(out, "\r\n"...)
	out = append(out, resp.body...)
	_, err := conn.Write(out)
	return err
}

// ---------------------------------------------------------------- routing helpers

// orderedParams is a query string parsed like URLSearchParams: insertion
// order preserved, get returns the first value, set replaces all.
type orderedParams struct {
	keys   []string
	values map[string][]string
}

func parseQueryParams(raw string) *orderedParams {
	p := &orderedParams{}
	if raw == "" {
		return p
	}
	p.values = map[string][]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			key, value = pair[:eq], pair[eq+1:]
		}
		key = queryUnescape(key)
		value = queryUnescape(value)
		if _, ok := p.values[key]; !ok {
			p.keys = append(p.keys, key)
		}
		p.values[key] = append(p.values[key], value)
	}
	return p
}

// queryUnescape decodes a query component: '+' becomes space, %XX decoded,
// malformed % sequences kept literally (like URLSearchParams).
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			out = append(out, ' ')
		case c == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]):
			out = append(out, hexVal(s[i+1])<<4|hexVal(s[i+2]))
			i += 2
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func (p *orderedParams) get(key string) (string, bool) {
	vals := p.values[key]
	if len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

func (p *orderedParams) set(key, value string) {
	if p.values == nil {
		p.values = map[string][]string{}
	}
	if _, ok := p.values[key]; !ok {
		p.keys = append(p.keys, key)
	}
	p.values[key] = []string{value}
}

func (p *orderedParams) clone() *orderedParams {
	out := &orderedParams{keys: append([]string(nil), p.keys...), values: map[string][]string{}}
	for k, v := range p.values {
		out.values[k] = append([]string(nil), v...)
	}
	return out
}

// queryEscape encodes a component the way URLSearchParams.toString() does:
// alphanumerics and *-._ pass through, space becomes '+', the rest is %XX.
func queryEscape(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '*':
			out = append(out, c)
		case c == ' ':
			out = append(out, '+')
		default:
			const hexd = "0123456789ABCDEF"
			out = append(out, '%', hexd[c>>4], hexd[c&0xf])
		}
	}
	return string(out)
}

func (p *orderedParams) encode() string {
	var parts []string
	for _, key := range p.keys {
		for _, value := range p.values[key] {
			parts = append(parts, queryEscape(key)+"="+queryEscape(value))
		}
	}
	return strings.Join(parts, "&")
}

// requestQuery is the query context for one request.
type requestQuery struct {
	params   *orderedParams
	host     string
	pathname string
}

// buildLinkHeader builds the Link header for a paginated list, with absolute same-origin URLs.
func buildLinkHeader(query *requestQuery, page, perPage, total int) string {
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	makeURL := func(p int) string {
		copy := query.params.clone()
		copy.set("page", itoa(p))
		copy.set("per_page", itoa(perPage))
		return "http://" + query.host + query.pathname + "?" + copy.encode()
	}
	var parts []string
	if page < totalPages {
		parts = append(parts, "<"+makeURL(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		parts = append(parts, "<"+makeURL(page-1)+">; rel=\"prev\"")
	}
	parts = append(parts, "<"+makeURL(totalPages)+">; rel=\"last\"")
	parts = append(parts, "<"+makeURL(1)+">; rel=\"first\"")
	return strings.Join(parts, ", ")
}

// decodeSegment decodes one path segment (decodeURIComponent semantics:
// errors leave the segment as-is).
func decodeSegment(segment string) string {
	if !strings.Contains(segment, "%") {
		return segment
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

func clampIntParam(params *orderedParams, key string, fallback, min, max int) int {
	raw, ok := params.get(key)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		// parseInt("12abc") is 12 in JS; Atoi rejects it. Match JS: leading digits.
		if n, ok := jsParseInt(raw); ok {
			value = n
		} else {
			return fallback
		}
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// resolveCommitTip resolves a ref-ish to a commit oid: a 40-hex oid, a full
// ref, a branch name, or a tag name. Annotated tags are peeled to their
// commit. "" when nothing resolves.
func resolveCommitTip(repo *Repo, ref string, hasRef bool) string {
	name := ref
	if !hasRef || ref == "" {
		name = repo.DefaultBranch
	}
	var oid string
	var ok bool
	if isObjectID(name) {
		if _, exists := repo.Objects[name]; exists {
			oid, ok = name, true
		}
	} else if strings.HasPrefix(name, "refs/") {
		oid, ok = repo.Refs[name]
	} else {
		oid, ok = repo.Refs["refs/heads/"+name]
		if !ok {
			oid, ok = repo.Refs["refs/tags/"+name]
		}
	}
	if !ok {
		return ""
	}
	for hops := 0; hops < 10; hops++ {
		object := repo.Objects[oid]
		if object == nil {
			return ""
		}
		if object.Type == objCommit {
			return oid
		}
		if object.Type != objTag {
			return ""
		}
		oid = parseTag(object.Data).Object
	}
	return ""
}

// parseWho parses a git identity line ("Name <email> ts tz") into its parts.
func parseWho(who string) (name, email, date string) {
	// ^(.*?) <(.*?)> (\d+) ([+-]\d{4})$
	lt := strings.Index(who, " <")
	if lt < 0 {
		return who, "", ""
	}
	gt := strings.Index(who[lt+2:], "> ")
	if gt < 0 {
		return who, "", ""
	}
	rest := who[lt+2+gt+2:]
	// rest must be "<digits> <+dddd|-dddd>"
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return who, "", ""
	}
	tsStr, tz := rest[:sp], rest[sp+1:]
	if len(tz) != 5 || tz[0] != '+' && tz[0] != '-' {
		return who, "", ""
	}
	for _, c := range tz[1:] {
		if c < '0' || c > '9' {
			return who, "", ""
		}
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return who, "", ""
	}
	for _, c := range tsStr {
		if c < '0' || c > '9' {
			return who, "", ""
		}
	}
	return who[:lt], who[lt+2 : lt+2+gt], isoFromUnix(ts)
}

func isoFromUnix(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// ---------------------------------------------------------------- response builders

func jsonResponse(status int, body []byte, requestID string, extra ...[2]string) httpResponse {
	headers := [][2]string{{"x-github-request-id", requestID}}
	headers = append(headers, extra...)
	return httpResponse{status: status, contentType: "application/json", body: body, extra: headers}
}

func jsonMessage(status int, message, requestID string, extra ...[2]string) httpResponse {
	var b []byte
	b = append(b, `{"message":`...)
	b = appendJSONString(b, message)
	b = append(b, '}')
	return jsonResponse(status, b, requestID, extra...)
}

func notFoundResponse(requestID string) httpResponse {
	return jsonMessage(404, "Not Found", requestID)
}

var oauthScopesHeader = [2]string{"x-oauth-scopes", "repo, read:org"}

// handle routes one request: read-only requests under a read lock,
// everything else under the write lock. Misclassification must err
// toward the write lock; a read-only path that took it would only be
// slower, never wrong.
func (a *app) handle(req *httpRequest) (resp httpResponse) {
	if isReadOnlyRequest(req) {
		a.mu.RLock()
		defer a.mu.RUnlock()
	} else {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	requestID := "req-" + itoa(int(a.requestCounter.Add(1)))
	defer func() {
		if r := recover(); r != nil {
			if ge, ok := r.(*GitHTTPError); ok {
				resp = jsonMessage(ge.Status, ge.Message, requestID)
				return
			}
			resp = jsonMessage(500, "Internal server error", requestID)
		}
	}()
	return a.route(req, requestID)
}

// isReadOnlyRequest is true for GET/HEAD and for POST git-upload-pack
// (ls-refs/fetch only read). Every other method, and every other POST
// endpoint (receive-pack, REST writes, GraphQL, MCP), may mutate.
func isReadOnlyRequest(req *httpRequest) bool {
	if req.method == "GET" || req.method == "HEAD" {
		return true
	}
	if req.method != "POST" {
		return false
	}
	path := req.rawURL
	if qAt := strings.IndexByte(path, '?'); qAt >= 0 {
		path = path[:qAt]
	}
	return strings.HasSuffix(path, ".git/git-upload-pack")
}

func (a *app) isAuthorized(req *httpRequest) bool {
	header := req.authorization
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := header[len("Bearer "):]
	if token == "" {
		return false
	}
	if a.expectedToken != "" {
		return token == a.expectedToken
	}
	return true
}

// readJSONBody parses a JSON body; an empty body is {} and a malformed one is a 400.
// Returns (nil, resp) when the 400 has already been built.
func readJSONBody(req *httpRequest, requestID string) (map[string]any, *httpResponse) {
	if len(req.body) == 0 {
		return map[string]any{}, nil
	}
	parsed, ok := unmarshalJSON(req.body)
	if !ok {
		resp := jsonMessage(400, "Problems parsing JSON", requestID)
		return nil, &resp
	}
	if m := asRecord(parsed); m != nil {
		return m, nil
	}
	return map[string]any{}, nil
}

// ---------------------------------------------------------------- routing

// matchGitPath matches /{owner}/{repo}.git/{endpoint} like the TypeScript
// GIT_PATH regex. Returns owner, repo, endpoint, matched.
func matchGitPath(pathname string) (string, string, string, bool) {
	if !strings.Contains(pathname, ".git/") {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(pathname, "/")
	slash1 := strings.IndexByte(rest, '/')
	if slash1 < 0 {
		return "", "", "", false
	}
	owner := rest[:slash1]
	rest = rest[slash1+1:]
	slash2 := strings.IndexByte(rest, '/')
	if slash2 < 0 {
		return "", "", "", false
	}
	repoSeg := rest[:slash2]
	endpoint := rest[slash2+1:]
	if owner == "" || !strings.HasSuffix(repoSeg, ".git") || len(repoSeg) <= 4 {
		return "", "", "", false
	}
	if endpoint != "info/refs" && endpoint != "git-upload-pack" && endpoint != "git-receive-pack" {
		return "", "", "", false
	}
	return owner, repoSeg[:len(repoSeg)-4], endpoint, true
}

// route dispatches one request (called under the app mutex).
func (a *app) route(req *httpRequest, requestID string) httpResponse {
	method := req.method
	host := req.host
	rawURL := req.rawURL
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		slashAt := strings.Index(rawURL[strings.Index(rawURL, "://")+3:], "/")
		if slashAt < 0 {
			rawURL = "/"
		} else {
			rawURL = rawURL[strings.Index(rawURL, "://")+3+slashAt:]
		}
	}
	qAt := strings.IndexByte(rawURL, '?')
	pathname := rawURL
	queryRaw := ""
	if qAt >= 0 {
		pathname = rawURL[:qAt]
		queryRaw = rawURL[qAt+1:]
	}
	if len(pathname) > 1 {
		pathname = strings.TrimRight(pathname, "/")
		if pathname == "" {
			pathname = "/"
		}
	}
	query := &requestQuery{params: parseQueryParams(queryRaw), host: host, pathname: pathname}
	var segments []string
	for _, s := range strings.Split(pathname, "/") {
		if s != "" {
			segments = append(segments, decodeSegment(s))
		}
	}

	// Public: the health probe the suite dials before anything else.
	if pathname == "/healthz" {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		return jsonResponse(200, []byte(`{"status":"ok"}`), requestID)
	}

	// Public: the API root (with the OAuth scopes header) and the meta endpoint.
	if pathname == "/api/v3" {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		return jsonResponse(200, []byte(`{"current_user_url":"/api/v3/user","user_url":"/api/v3/user","meta_url":"/api/v3/meta"}`), requestID, oauthScopesHeader)
	}
	if pathname == "/api/v3/meta" {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		return jsonResponse(200, []byte(`{"installed_version":"1.0.0"}`), requestID)
	}

	// MCP: JSON-RPC over Streamable HTTP. Auth failure is a JSON-RPC error body.
	if pathname == "/mcp" {
		if method != "POST" {
			return jsonMessage(405, "Method not allowed", requestID)
		}
		if !a.isAuthorized(req) {
			return jsonResponse(401, marshalJSON(mcpUnauthorized()), requestID)
		}
		outcome := handleMcp(a.forge, req.body)
		if outcome.Status == 202 {
			return httpResponse{status: 202, noContentType: true, extra: [][2]string{{"x-github-request-id", requestID}}}
		}
		return jsonResponse(outcome.Status, marshalJSON(outcome.JSON), requestID)
	}

	// GraphQL.
	if pathname == "/api/graphql" {
		if method != "POST" {
			return notFoundResponse(requestID)
		}
		if !a.isAuthorized(req) {
			return jsonMessage(401, "Bad credentials", requestID)
		}
		var parsed any
		if len(req.body) == 0 {
			parsed = map[string]any{}
		} else {
			var ok bool
			parsed, ok = unmarshalJSON(req.body)
			if !ok {
				return jsonMessage(400, "Problems parsing JSON", requestID)
			}
		}
		return jsonResponse(200, marshalJSON(handleGraphQL(a.forge, parsed)), requestID)
	}

	// Git smart-HTTP.
	if owner, repoName, endpoint, ok := matchGitPath(pathname); ok {
		owner = decodeSegment(owner)
		repoName = decodeSegment(repoName)
		if !a.isAuthorized(req) {
			return jsonMessage(401, "Bad credentials", requestID)
		}
		repo := a.forge.GetRepo(owner, repoName)
		if repo == nil {
			return notFoundResponse(requestID)
		}
		sendGit := func(gr GitHTTPResponse) httpResponse {
			return httpResponse{status: gr.Status, contentType: gr.ContentType, body: gr.Body, extra: [][2]string{{"x-github-request-id", requestID}}}
		}
		if endpoint == "info/refs" {
			if method != "GET" {
				return notFoundResponse(requestID)
			}
			service, _ := query.params.get("service")
			gr, err := handleInfoRefs(repo, service, req.gitProtocol)
			if err != nil {
				if ge, ok := err.(*GitHTTPError); ok {
					return jsonMessage(ge.Status, ge.Message, requestID)
				}
				return jsonMessage(500, "Internal server error", requestID)
			}
			return sendGit(gr)
		}
		if endpoint == "git-upload-pack" {
			if method != "POST" {
				return notFoundResponse(requestID)
			}
			gr, err := handleUploadPack(a.forge, repo, req.body, req.gitProtocol)
			if err != nil {
				if ge, ok := err.(*GitHTTPError); ok {
					return jsonMessage(ge.Status, ge.Message, requestID)
				}
				return jsonMessage(500, "Internal server error", requestID)
			}
			return sendGit(gr)
		}
		if method != "POST" {
			return notFoundResponse(requestID)
		}
		gr, err := handleReceivePack(a.forge, repo, req.body)
		if err != nil {
			if ge, ok := err.(*GitHTTPError); ok {
				return jsonMessage(ge.Status, ge.Message, requestID)
			}
			return jsonMessage(500, "Internal server error", requestID)
		}
		return sendGit(gr)
	}

	// REST under /api/v3 (everything below the root and meta, which are public above).
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !a.isAuthorized(req) {
			return jsonMessage(401, "Bad credentials", requestID)
		}
		rest := segments[2:]
		if len(rest) == 1 && rest[0] == "user" {
			if method != "GET" {
				return notFoundResponse(requestID)
			}
			var b []byte
			b = append(b, '{')
			b = appendJSONKey(b, "login")
			b = appendJSONString(b, viewer.Login)
			b = append(b, ',')
			b = appendJSONKey(b, "id")
			b = appendJSONInt(b, viewer.ID)
			b = append(b, ',')
			b = appendJSONKey(b, "name")
			b = appendJSONString(b, viewer.Name)
			b = append(b, ',')
			b = appendJSONKey(b, "email")
			b = appendJSONString(b, viewer.Email)
			b = append(b, ',')
			b = appendJSONKey(b, "type")
			b = appendJSONString(b, "User")
			b = append(b, '}')
			return jsonResponse(200, b, requestID, oauthScopesHeader)
		}
		if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
			if method == "GET" {
				repos := a.forge.ListRepos(viewer.Login)
				perPage := clampIntParam(query.params, "per_page", 30, 1, 100)
				page := clampIntParam(query.params, "page", 1, 1, int(^uint(0)>>1))
				start := (page - 1) * perPage
				end := start + perPage
				if start > len(repos) {
					start = len(repos)
				}
				if end > len(repos) {
					end = len(repos)
				}
				var b []byte
				b = append(b, '[')
				for i, repo := range repos[start:end] {
					if i > 0 {
						b = append(b, ',')
					}
					b = append(b, a.forge.RepoJSON(repo)...)
				}
				b = append(b, ']')
				link := buildLinkHeader(query, page, perPage, len(repos))
				return jsonResponse(200, b, requestID, oauthScopesHeader, [2]string{"link", link})
			}
			if method == "POST" {
				body, errResp := readJSONBody(req, requestID)
				if errResp != nil {
					return *errResp
				}
				name := recordString(body, "name")
				if name == "" {
					return jsonMessage(422, "name is required", requestID)
				}
				if a.forge.GetRepo(viewer.Login, name) != nil {
					return jsonMessage(422, "Repository "+viewer.Login+"/"+name+" already exists", requestID)
				}
				defaultBranch := recordString(body, "default_branch")
				if defaultBranch == "" {
					defaultBranch = "main"
				}
				repo := a.forge.CreateRepo(viewer.Login, name, recordBool(body, "private"), recordString(body, "description"), defaultBranch)
				return jsonResponse(201, a.forge.RepoJSON(repo), requestID, oauthScopesHeader)
			}
			return notFoundResponse(requestID)
		}
		if len(rest) >= 3 && rest[0] == "repos" {
			owner := rest[1]
			repoName := rest[2]
			repo := a.forge.GetRepo(owner, repoName)
			if repo == nil {
				return notFoundResponse(requestID)
			}
			return a.handleRepoRoute(req, query, repo, rest[3:], method, requestID)
		}
		return notFoundResponse(requestID)
	}

	return notFoundResponse(requestID)
}

// ---------------------------------------------------------------- repo-scoped REST

func (a *app) handleRepoRoute(req *httpRequest, query *requestQuery, repo *Repo, sub []string, method, requestID string) httpResponse {
	needsBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
	var body map[string]any
	if needsBody {
		var errResp *httpResponse
		body, errResp = readJSONBody(req, requestID)
		if errResp != nil {
			return *errResp
		}
	} else {
		body = map[string]any{}
	}
	lookup := a.forge.Lookup(repo)
	store := repoStore{a.forge, repo}

	// GET/PATCH/DELETE /repos/{o}/{r}
	if len(sub) == 0 {
		if method == "GET" {
			return jsonResponse(200, a.forge.RepoJSON(repo), requestID, oauthScopesHeader)
		}
		if method == "PATCH" {
			if v, ok := body["description"].(string); ok {
				repo.Description = v
			}
			if v, ok := body["private"].(bool); ok {
				repo.Private = v
			}
			if v, ok := body["default_branch"].(string); ok && v != "" {
				repo.DefaultBranch = v
			}
			repo.touch()
			return jsonResponse(200, a.forge.RepoJSON(repo), requestID, oauthScopesHeader)
		}
		if method == "DELETE" {
			a.forge.DeleteRepo(repo.Owner, repo.Name)
			return httpResponse{status: 204, contentType: "application/json", extra: [][2]string{{"x-github-request-id", requestID}}}
		}
		return notFoundResponse(requestID)
	}

	// POST /repos/{o}/{r}/git/refs — create a branch or tag at a sha.
	if sub[0] == "git" && len(sub) >= 2 && sub[1] == "refs" && len(sub) == 2 {
		if method != "POST" {
			return notFoundResponse(requestID)
		}
		ref := recordString(body, "ref")
		sha := recordString(body, "sha")
		if ref == "" || !strings.HasPrefix(ref, "refs/") {
			return jsonMessage(422, "ref must start with refs/", requestID)
		}
		if sha == "" || !isObjectID(sha) {
			return jsonMessage(422, "sha is required and must be an object id", requestID)
		}
		object := lookup(sha)
		if object == nil {
			return jsonMessage(422, "Object "+sha+" does not exist", requestID)
		}
		if _, exists := repo.Refs[ref]; exists {
			return jsonMessage(422, "Reference "+ref+" already exists", requestID)
		}
		repo.Refs[ref] = sha
		repo.touch()
		return jsonResponse(201, marshalJSON(map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": string(object.Type)}}), requestID, oauthScopesHeader)
	}

	// GET /repos/{o}/{r}/git/refs/heads/{branch} (the ref path may itself contain slashes).
	if sub[0] == "git" && len(sub) >= 2 && sub[1] == "refs" && len(sub) > 2 {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		ref := "refs/" + strings.Join(sub[2:], "/")
		sha, ok := repo.Refs[ref]
		if !ok {
			return notFoundResponse(requestID)
		}
		object := lookup(sha)
		typ := "commit"
		if object != nil {
			typ = string(object.Type)
		}
		return jsonResponse(200, marshalJSON(map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": typ}}), requestID, oauthScopesHeader)
	}

	// GET /repos/{o}/{r}/branches
	if sub[0] == "branches" && len(sub) == 1 {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		type branch struct {
			name string
			sha  string
		}
		var branches []branch
		for name, sha := range repo.Refs {
			if strings.HasPrefix(name, "refs/heads/") {
				branches = append(branches, branch{name: name[len("refs/heads/"):], sha: sha})
			}
		}
		sortSlice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
		var b []byte
		b = append(b, '[')
		for i, br := range branches {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, '{')
			b = appendJSONKey(b, "name")
			b = appendJSONString(b, br.name)
			b = append(b, ',')
			b = appendJSONKey(b, "commit")
			b = append(b, '{')
			b = appendJSONKey(b, "sha")
			b = appendJSONString(b, br.sha)
			b = append(b, '}', '}')
		}
		b = append(b, ']')
		return jsonResponse(200, b, requestID, oauthScopesHeader)
	}

	// GET /repos/{o}/{r}/branches/{branch}
	if sub[0] == "branches" && len(sub) > 1 {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		branch := strings.Join(sub[1:], "/")
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return notFoundResponse(requestID)
		}
		var b []byte
		b = append(b, '{')
		b = appendJSONKey(b, "name")
		b = appendJSONString(b, branch)
		b = append(b, ',')
		b = appendJSONKey(b, "commit")
		b = append(b, '{')
		b = appendJSONKey(b, "sha")
		b = appendJSONString(b, sha)
		b = append(b, '}', '}')
		return jsonResponse(200, b, requestID, oauthScopesHeader)
	}

	// GET /repos/{o}/{r}/commits[?sha=<ref>]
	if sub[0] == "commits" && len(sub) == 1 {
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		shaParam, hasSha := query.params.get("sha")
		tip := resolveCommitTip(repo, shaParam, hasSha)
		if tip == "" {
			if !hasSha {
				return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader) // an empty repo
			}
			return notFoundResponse(requestID)
		}
		oids := listCommits(lookup, tip, 100)
		var out []any
		for _, oid := range oids {
			var data []byte
			if o := lookup(oid); o != nil {
				data = o.Data
			}
			commit := parseCommit(data)
			aName, aEmail, aDate := parseWho(commit.Author)
			cName, cEmail, cDate := parseWho(commit.Committer)
			out = append(out, map[string]any{
				"sha": oid,
				"commit": map[string]any{
					"message":   strings.TrimRight(commit.Message, "\n"),
					"author":    map[string]any{"name": aName, "email": aEmail, "date": aDate},
					"committer": map[string]any{"name": cName, "email": cEmail, "date": cDate},
				},
			})
		}
		if out == nil {
			return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader)
		}
		return jsonResponse(200, marshalJSON(out), requestID, oauthScopesHeader)
	}

	// /repos/{o}/{r}/contents/{path}
	if sub[0] == "contents" {
		return a.handleContents(req, query, repo, lookup, store, sub[1:], body, method, requestID)
	}

	// GET/POST /repos/{o}/{r}/labels
	if sub[0] == "labels" && len(sub) == 1 {
		if method == "GET" {
			var b []byte
			b = append(b, '[')
			first := true
			for _, name := range repo.LabelOrder {
				label := repo.Labels[name]
				if label == nil {
					continue
				}
				if !first {
					b = append(b, ',')
				}
				first = false
				b = append(b, '{')
				b = appendJSONKey(b, "name")
				b = appendJSONString(b, label.Name)
				b = append(b, ',')
				b = appendJSONKey(b, "color")
				b = appendJSONString(b, label.Color)
				b = append(b, '}')
			}
			b = append(b, ']')
			return jsonResponse(200, b, requestID, oauthScopesHeader)
		}
		if method == "POST" {
			name := recordString(body, "name")
			if name == "" {
				return jsonMessage(422, "name is required", requestID)
			}
			if _, exists := repo.Labels[name]; exists {
				return jsonMessage(422, "Label "+name+" already exists", requestID)
			}
			color := recordString(body, "color")
			if color == "" {
				color = "000000"
			}
			repo.Labels[name] = &Label{Name: name, Color: color}
			repo.LabelOrder = append(repo.LabelOrder, name)
			repo.touch()
			var b []byte
			b = append(b, '{')
			b = appendJSONKey(b, "name")
			b = appendJSONString(b, name)
			b = append(b, ',')
			b = appendJSONKey(b, "color")
			b = appendJSONString(b, color)
			b = append(b, '}')
			return jsonResponse(201, b, requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET/POST /repos/{o}/{r}/issues
	if sub[0] == "issues" && len(sub) == 1 {
		if method == "GET" {
			state, _ := query.params.get("state")
			if state == "" {
				state = "open"
			}
			sortParam, _ := query.params.get("sort")
			if sortParam == "" {
				sortParam = "created"
			}
			direction, _ := query.params.get("direction")
			if direction == "" {
				direction = "desc"
			}
			var issues []*Issue
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			if state == "open" {
				issues = filterIssues(issues, func(i *Issue) bool { return i.State == "open" })
			} else if state == "closed" {
				issues = filterIssues(issues, func(i *Issue) bool { return i.State == "closed" })
			}
			dir := -1
			if direction == "asc" {
				dir = 1
			}
			if sortParam == "updated" {
				sortSliceStable(issues, func(i, j int) bool { return (issues[i].UpdatedSeq-issues[j].UpdatedSeq)*dir < 0 })
			} else {
				sortSliceStable(issues, func(i, j int) bool { return (issues[i].Number-issues[j].Number)*dir < 0 })
			}
			perPage := clampIntParam(query.params, "per_page", 30, 1, 100)
			page := clampIntParam(query.params, "page", 1, 1, int(^uint(0)>>1))
			start := (page - 1) * perPage
			end := start + perPage
			if start > len(issues) {
				start = len(issues)
			}
			if end > len(issues) {
				end = len(issues)
			}
			var b []byte
			b = append(b, '[')
			for i, issue := range issues[start:end] {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, a.forge.IssueJSON(repo, issue)...)
			}
			b = append(b, ']')
			link := buildLinkHeader(query, page, perPage, len(issues))
			return jsonResponse(200, b, requestID, oauthScopesHeader, [2]string{"link", link})
		}
		if method == "POST" {
			title := recordString(body, "title")
			if title == "" {
				return jsonMessage(422, "title is required", requestID)
			}
			bodyText := recordString(body, "body")
			var labels []*Label
			if arr, ok := body["labels"].([]any); ok {
				for _, name := range arr {
					nameStr, ok := name.(string)
					if !ok {
						continue
					}
					label := repo.Labels[nameStr]
					if label == nil {
						return jsonMessage(422, "Label "+nameStr+" does not exist", requestID)
					}
					labels = append(labels, label)
				}
			}
			issue := a.forge.CreateIssue(repo, title, bodyText, labels)
			return jsonResponse(201, a.forge.IssueJSON(repo, issue), requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET/PATCH /repos/{o}/{r}/issues/{n}
	if sub[0] == "issues" && len(sub) == 2 {
		number, ok := parsePathNumber(sub[1])
		if !ok {
			return notFoundResponse(requestID)
		}
		issue := repo.Issues[number]
		if issue == nil {
			return notFoundResponse(requestID)
		}
		if method == "GET" {
			return jsonResponse(200, a.forge.IssueJSON(repo, issue), requestID, oauthScopesHeader)
		}
		if method == "PATCH" {
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				issue.State = v
			}
			if v, ok := body["title"].(string); ok {
				issue.Title = v
			}
			if v, ok := body["body"].(string); ok {
				issue.Body = v
			}
			issue.UpdatedSeq = a.forge.NextSeq()
			repo.touch()
			return jsonResponse(200, a.forge.IssueJSON(repo, issue), requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET/POST /repos/{o}/{r}/issues/{n}/comments
	if sub[0] == "issues" && len(sub) == 3 && sub[2] == "comments" {
		number, ok := parsePathNumber(sub[1])
		if !ok {
			return notFoundResponse(requestID)
		}
		issue := repo.Issues[number]
		if issue == nil {
			return notFoundResponse(requestID)
		}
		if method == "GET" {
			var b []byte
			b = append(b, '[')
			for i, comment := range issue.Comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, a.forge.CommentJSON(comment)...)
			}
			b = append(b, ']')
			return jsonResponse(200, b, requestID, oauthScopesHeader)
		}
		if method == "POST" {
			text := recordString(body, "body")
			if text == "" {
				return jsonMessage(422, "body is required", requestID)
			}
			comment := a.forge.CreateComment(repo, issue, text)
			return jsonResponse(201, a.forge.CommentJSON(comment), requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET/POST /repos/{o}/{r}/pulls
	if sub[0] == "pulls" && len(sub) == 1 {
		if method == "GET" {
			state, _ := query.params.get("state")
			if state == "" {
				state = "open"
			}
			var pulls []*Pull
			for _, pull := range repo.Pulls {
				pulls = append(pulls, pull)
			}
			if state == "open" {
				pulls = filterPulls(pulls, func(p *Pull) bool { return p.State == "open" })
			} else if state == "closed" {
				pulls = filterPulls(pulls, func(p *Pull) bool { return p.State == "closed" })
			}
			sortSliceStable(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
			perPage := clampIntParam(query.params, "per_page", 30, 1, 100)
			page := clampIntParam(query.params, "page", 1, 1, int(^uint(0)>>1))
			start := (page - 1) * perPage
			end := start + perPage
			if start > len(pulls) {
				start = len(pulls)
			}
			if end > len(pulls) {
				end = len(pulls)
			}
			var b []byte
			b = append(b, '[')
			for i, pull := range pulls[start:end] {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, a.forge.PullJSON(repo, pull)...)
			}
			b = append(b, ']')
			link := buildLinkHeader(query, page, perPage, len(pulls))
			return jsonResponse(200, b, requestID, oauthScopesHeader, [2]string{"link", link})
		}
		if method == "POST" {
			title := recordString(body, "title")
			head := recordString(body, "head")
			base := recordString(body, "base")
			if base == "" {
				base = repo.DefaultBranch
			}
			if title == "" {
				return jsonMessage(422, "title is required", requestID)
			}
			if head == "" {
				return jsonMessage(422, "head is required", requestID)
			}
			headSha, okH := repo.Refs["refs/heads/"+head]
			baseSha, okB := repo.Refs["refs/heads/"+base]
			if !okH {
				return jsonMessage(422, "head branch "+head+" not found", requestID)
			}
			if !okB {
				return jsonMessage(422, "base branch "+base+" not found", requestID)
			}
			duplicate := false
			for _, pull := range repo.Pulls {
				if pull.State == "open" && pull.HeadRef == head && pull.BaseRef == base {
					duplicate = true
					break
				}
			}
			if duplicate {
				return jsonMessage(422, "A pull request already exists for these branches", requestID)
			}
			bodyText := recordString(body, "body")
			pull := a.forge.CreatePull(repo, title, bodyText, head, base, headSha, baseSha)
			return jsonResponse(201, a.forge.PullJSON(repo, pull), requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET /repos/{o}/{r}/pulls/{n}
	if sub[0] == "pulls" && len(sub) == 2 {
		number, ok := parsePathNumber(sub[1])
		if !ok {
			return notFoundResponse(requestID)
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return notFoundResponse(requestID)
		}
		if method == "GET" {
			return jsonResponse(200, a.forge.PullJSON(repo, pull), requestID, oauthScopesHeader)
		}
		return notFoundResponse(requestID)
	}

	// GET /repos/{o}/{r}/pulls/{n}/files
	if sub[0] == "pulls" && len(sub) == 3 && sub[2] == "files" {
		number, ok := parsePathNumber(sub[1])
		if !ok {
			return notFoundResponse(requestID)
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return notFoundResponse(requestID)
		}
		if method != "GET" {
			return notFoundResponse(requestID)
		}
		headSha, baseSha := a.forge.PullShas(repo, pull)
		headCommit := lookup(headSha)
		baseCommit := lookup(baseSha)
		if headCommit == nil || baseCommit == nil {
			return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader)
		}
		base := mergeBase(lookup, baseSha, headSha)
		baseTree := ""
		if base != "" {
			if o := lookup(base); o != nil {
				baseTree = parseCommit(o.Data).Tree
			}
		}
		diffs := diffTrees(lookup, baseTree, parseCommit(headCommit.Data).Tree)
		var out []any
		for _, diff := range diffs {
			status := "modified"
			if diff.Status == "added" {
				status = "added"
			} else if diff.Status == "removed" {
				status = "changed"
			}
			sha := diff.NewOid
			if sha == "" {
				sha = diff.OldOid
			}
			out = append(out, map[string]any{
				"filename": diff.Path, "status": status,
				"additions": diff.Additions, "deletions": diff.Deletions,
				"changes": diff.Changes, "sha": sha,
			})
		}
		if out == nil {
			return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader)
		}
		return jsonResponse(200, marshalJSON(out), requestID, oauthScopesHeader)
	}

	// PUT /repos/{o}/{r}/pulls/{n}/merge
	if sub[0] == "pulls" && len(sub) == 3 && sub[2] == "merge" {
		number, ok := parsePathNumber(sub[1])
		if !ok {
			return notFoundResponse(requestID)
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return notFoundResponse(requestID)
		}
		if method != "PUT" {
			return notFoundResponse(requestID)
		}
		methodRaw := recordString(body, "merge_method")
		if methodRaw == "" {
			methodRaw = "merge"
		}
		mergeMethod := "squash"
		if methodRaw == "merge" {
			mergeMethod = "merge"
		}
		sha, conflict := a.forge.MergePull(repo, pull, mergeMethod)
		if sha == "" {
			if conflict {
				return jsonMessage(405, "Merge conflict", requestID)
			}
			return jsonMessage(405, "Pull request cannot be merged", requestID)
		}
		return jsonResponse(200, marshalJSON(map[string]any{"merged": true, "sha": sha, "message": "Pull Request successfully merged"}), requestID, oauthScopesHeader)
	}

	return notFoundResponse(requestID)
}

// handleContents serves /repos/{o}/{r}/contents/{path}.
func (a *app) handleContents(req *httpRequest, query *requestQuery, repo *Repo, lookup ObjectLookup, store repoStore, sub []string, body map[string]any, method, requestID string) httpResponse {
	path := strings.Join(sub, "/")
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if method == "GET" {
		refParam, hasRef := query.params.get("ref")
		tip := resolveCommitTip(repo, refParam, hasRef)
		if tip == "" {
			return notFoundResponse(requestID)
		}
		var data []byte
		if o := lookup(tip); o != nil {
			data = o.Data
		}
		tree := parseCommit(data).Tree
		dirEntry := func(entry TreeEntry, parentPath string) map[string]any {
			entryPath := entry.Name
			if parentPath != "" {
				entryPath = parentPath + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				return map[string]any{"type": "dir", "size": 0, "name": entry.Name, "path": entryPath, "sha": entry.Oid}
			}
			size := 0
			if object := lookup(entry.Oid); object != nil {
				size = len(object.Data)
			}
			return map[string]any{"type": "file", "encoding": "base64", "size": size, "name": entry.Name, "path": entryPath, "sha": entry.Oid}
		}
		if len(segments) == 0 {
			entries := treeListDir(lookup, tree, nil)
			var out []any
			for _, entry := range entries {
				out = append(out, dirEntry(entry, ""))
			}
			if out == nil {
				return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader)
			}
			return jsonResponse(200, marshalJSON(out), requestID, oauthScopesHeader)
		}
		found := treeGetPath(lookup, tree, segments)
		if found == nil {
			return notFoundResponse(requestID)
		}
		if found.entry.Mode == "40000" {
			entries := treeListDir(lookup, tree, segments)
			var out []any
			for _, entry := range entries {
				out = append(out, dirEntry(entry, path))
			}
			if out == nil {
				return jsonResponse(200, []byte("[]"), requestID, oauthScopesHeader)
			}
			return jsonResponse(200, marshalJSON(out), requestID, oauthScopesHeader)
		}
		blob := lookup(found.oid)
		if blob == nil {
			return notFoundResponse(requestID)
		}
		return jsonResponse(200, marshalJSON(map[string]any{
			"type": "file", "encoding": "base64", "size": len(blob.Data),
			"name": segments[len(segments)-1], "path": path,
			"content": base64Encode(blob.Data), "sha": found.oid,
		}), requestID, oauthScopesHeader)
	}
	if method == "PUT" {
		message := recordString(body, "message")
		if message == "" {
			return jsonMessage(422, "message is required", requestID)
		}
		contentB64, ok := body["content"].(string)
		if !ok {
			return jsonMessage(422, "content is required", requestID)
		}
		stripped := stripWhitespace(contentB64)
		content, err := base64Decode(stripped)
		if err != nil || base64Encode(content) != stripped {
			return jsonMessage(422, "content is not valid base64", requestID)
		}
		if len(segments) == 0 {
			return jsonMessage(422, "a file path is required", requestID)
		}
		branch := recordString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		sha := recordString(body, "sha")
		// A branch that does not exist yet starts from the default branch's tip.
		tip, hasTip := repo.Refs["refs/heads/"+branch]
		if !hasTip {
			tip, hasTip = repo.Refs["refs/heads/"+repo.DefaultBranch]
		}
		currentTree := ""
		hasTree := false
		if hasTip {
			if o := lookup(tip); o != nil {
				currentTree = parseCommit(o.Data).Tree
				hasTree = true
			}
		}
		var existing *treeFound
		if hasTree {
			existing = treeGetPath(lookup, currentTree, segments)
		}
		status := 201
		if existing != nil {
			status = 200
			if sha == "" {
				return jsonMessage(422, "sha is required to update a file", requestID)
			}
			if sha != existing.oid {
				return jsonMessage(409, "sha does not match the current file", requestID)
			}
		}
		blobOid := a.forge.PutObject(repo, objBlob, content)
		newTree := treeSetPath(lookup, store, currentTree, segments, blobOid)
		var parents []string
		if hasTip {
			parents = []string{tip}
		}
		commitSha := a.forge.CreateCommit(repo, newTree, parents, message)
		repo.Refs["refs/heads/"+branch] = commitSha
		repo.touch()
		return jsonResponse(status, marshalJSON(map[string]any{
			"content": map[string]any{
				"name": segments[len(segments)-1], "path": path, "sha": blobOid,
				"size": len(content), "encoding": "base64", "content": contentB64,
			},
			"commit": map[string]any{"sha": commitSha},
		}), requestID, oauthScopesHeader)
	}
	if method == "DELETE" {
		message := recordString(body, "message")
		sha := recordString(body, "sha")
		if message == "" {
			return jsonMessage(422, "message is required", requestID)
		}
		if sha == "" {
			return jsonMessage(422, "sha is required", requestID)
		}
		branch := recordString(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip, hasTip := repo.Refs["refs/heads/"+branch]
		if !hasTip {
			return notFoundResponse(requestID)
		}
		var data []byte
		if o := lookup(tip); o != nil {
			data = o.Data
		}
		currentTree := parseCommit(data).Tree
		existing := treeGetPath(lookup, currentTree, segments)
		if existing == nil {
			return notFoundResponse(requestID)
		}
		if existing.entry.Mode == "40000" {
			return jsonMessage(422, "cannot delete a directory", requestID)
		}
		if sha != existing.oid {
			return jsonMessage(409, "sha does not match the current file", requestID)
		}
		newTree := treeSetPath(lookup, store, currentTree, segments, "")
		commitSha := a.forge.CreateCommit(repo, newTree, []string{tip}, message)
		repo.Refs["refs/heads/"+branch] = commitSha
		repo.touch()
		return jsonResponse(200, marshalJSON(map[string]any{"commit": map[string]any{"sha": commitSha}, "content": nil}), requestID, oauthScopesHeader)
	}
	return notFoundResponse(requestID)
}

// parsePathNumber parses a path segment the way Number() does, requiring an integer.
func parsePathNumber(s string) (int, bool) {
	f, ok := jsParseNumber(s)
	if !ok || !isIntegerFloat(f) {
		return 0, false
	}
	return int(f), true
}

func filterIssues(in []*Issue, keep func(*Issue) bool) []*Issue {
	out := in[:0]
	for _, item := range in {
		if keep(item) {
			out = append(out, item)
		}
	}
	return out
}

func filterPulls(in []*Pull, keep func(*Pull) bool) []*Pull {
	out := in[:0]
	for _, item := range in {
		if keep(item) {
			out = append(out, item)
		}
	}
	return out
}

// sortSlice sorts a slice (unstable).
func sortSlice[T any](s []T, less func(i, j int) bool) {
	sort.Slice(s, less)
}

// sortSliceStable sorts a slice stably.
func sortSliceStable[T any](s []T, less func(i, j int) bool) {
	sort.SliceStable(s, less)
}

// base64Encode encodes standard base64 (with padding).
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// base64Decode decodes standard base64.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// stripWhitespace removes all ASCII whitespace from s.
func stripWhitespace(s string) string {
	if !strings.ContainsAny(s, " \t\n\r\v\f") {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			out = append(out, c)
		}
	}
	return string(out)
}
