// Fast, allocation-light JSON writing for the hot REST views, plus the small
// JavaScript-number helpers the port needs to match the TypeScript semantics
// (JSON.parse/Number/parseInt) where the suite can observe the difference.

package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// appendJSONString appends s as a JSON string literal (with quotes), escaping
// exactly what JSON requires: `"`, `\`, and control characters below 0x20.
// Unlike encoding/json it does not escape `<`, `>` or `&` (matching
// JSON.stringify), and non-ASCII UTF-8 passes through unchanged.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0')
				const hexd = "0123456789abcdef"
				dst = append(dst, hexd[c>>4], hexd[c&0xf])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// appendJSONKey appends `"key":` to dst.
func appendJSONKey(dst []byte, key string) []byte {
	dst = appendJSONString(dst, key)
	return append(dst, ':')
}

func appendJSONInt(dst []byte, n int) []byte {
	return strconv.AppendInt(dst, int64(n), 10)
}

func appendJSONBool(dst []byte, b bool) []byte {
	if b {
		return append(dst, "true"...)
	}
	return append(dst, "false"...)
}

// extractIssueCreate pulls title and body out of a POST /issues body that is
// a flat JSON object whose values are all strings and whose keys are only
// "title" and "body" (the shape the write workload sends). It is a strict
// scanner, not a JSON parser: anything it cannot prove is that shape —
// another key, a non-string value, a bad escape, invalid UTF-8, trailing
// data — returns ok=false and the caller falls back to the generic
// encoding/json parse, which decides the real answer (including the 400 for
// malformed JSON). Duplicate keys resolve last-wins, like the generic map.
func extractIssueCreate(data []byte) (title, body string, ok bool) {
	i := 0
	skipWS := func() {
		for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
			i++
		}
	}
	skipWS()
	if i >= len(data) || data[i] != '{' {
		return "", "", false
	}
	i++
	skipWS()
	if i < len(data) && data[i] == '}' {
		i++
		skipWS()
		if i != len(data) {
			return "", "", false
		}
		return "", "", true
	}
	for {
		skipWS()
		key, next, good := scanJSONString(data, i)
		if !good {
			return "", "", false
		}
		i = next
		skipWS()
		if i >= len(data) || data[i] != ':' {
			return "", "", false
		}
		i++
		skipWS()
		value, next, good := scanJSONString(data, i)
		if !good {
			return "", "", false
		}
		i = next
		switch key {
		case "title":
			title = value
		case "body":
			body = value
		default:
			return "", "", false
		}
		skipWS()
		if i >= len(data) {
			return "", "", false
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == '}' {
			i++
			skipWS()
			if i != len(data) {
				return "", "", false
			}
			return title, body, true
		}
		return "", "", false
	}
}

// scanJSONString parses one JSON string literal starting at data[i] == '"'
// and returns its unescaped value and the offset just past the closing quote.
// ok=false on any malformed input (unterminated string, bad escape, a raw
// control character, invalid UTF-8 — the generic parser replaces that with
// U+FFFD, so the fast path declines it instead of diverging).
func scanJSONString(data []byte, i int) (string, int, bool) {
	if i >= len(data) || data[i] != '"' {
		return "", 0, false
	}
	i++
	start := i
	// Fast lane: no escape in the string.
	for i < len(data) {
		c := data[i]
		if c == '"' {
			s := string(data[start:i])
			if !utf8.ValidString(s) {
				return "", 0, false
			}
			return s, i + 1, true
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", 0, false
		}
		i++
	}
	if i >= len(data) {
		return "", 0, false
	}
	// Slow lane: at least one escape; rebuild the value.
	out := make([]byte, 0, 64)
	out = append(out, data[start:i]...)
	for i < len(data) {
		c := data[i]
		if c == '"' {
			s := string(out)
			if !utf8.ValidString(s) {
				return "", 0, false
			}
			return s, i + 1, true
		}
		if c < 0x20 {
			return "", 0, false
		}
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		i++
		if i >= len(data) {
			return "", 0, false
		}
		switch data[i] {
		case '"':
			out = append(out, '"')
			i++
		case '\\':
			out = append(out, '\\')
			i++
		case '/':
			out = append(out, '/')
			i++
		case 'b':
			out = append(out, '\b')
			i++
		case 'f':
			out = append(out, '\f')
			i++
		case 'n':
			out = append(out, '\n')
			i++
		case 'r':
			out = append(out, '\r')
			i++
		case 't':
			out = append(out, '\t')
			i++
		case 'u':
			r, next, good := scanUnicodeEscape(data, i+1)
			if !good {
				return "", 0, false
			}
			out = utf8.AppendRune(out, r)
			i = next
		default:
			return "", 0, false
		}
	}
	return "", 0, false
}

// scanUnicodeEscape decodes \uXXXX at data[i] (i points just past the 'u'),
// combining a surrogate pair when one follows. A lone surrogate decodes to
// U+FFFD, matching encoding/json.
func scanUnicodeEscape(data []byte, i int) (rune, int, bool) {
	hex4 := func(j int) (int, bool) {
		if j+4 > len(data) {
			return 0, false
		}
		v := 0
		for k := 0; k < 4; k++ {
			c := data[j+k]
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= int(c - '0')
			case c >= 'a' && c <= 'f':
				v |= int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v |= int(c-'A') + 10
			default:
				return 0, false
			}
		}
		return v, true
	}
	v, ok := hex4(i)
	if !ok {
		return 0, 0, false
	}
	i += 4
	if v >= 0xD800 && v <= 0xDBFF {
		// High surrogate: a following \uDC00-\uDFFF completes the pair.
		if i+6 <= len(data) && data[i] == '\\' && data[i+1] == 'u' {
			lo, ok := hex4(i + 2)
			if ok && lo >= 0xDC00 && lo <= 0xDFFF {
				return 0x10000 + rune(v-0xD800)<<10 + rune(lo-0xDC00), i + 6, true
			}
		}
		return utf8.RuneError, i, true
	}
	if v >= 0xDC00 && v <= 0xDFFF {
		return utf8.RuneError, i, true
	}
	return rune(v), i, true
}

// marshalJSON is encoding/json for the cold paths (GraphQL, MCP). Map keys
// come out sorted, so output is deterministic across calls.
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// unmarshalJSON parses a JSON document into any (numbers become float64).
// Returns nil, false when the document is malformed.
func unmarshalJSON(data []byte) (any, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return convertNumbers(v), true
}

// convertNumbers walks a decoded value turning json.Number into float64,
// matching JavaScript's number type (and keeping integers exact up to 2^53).
func convertNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0.0
		}
		return f
	case map[string]any:
		for k, e := range t {
			t[k] = convertNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = convertNumbers(e)
		}
		return t
	default:
		return v
	}
}

// asRecord returns v as a JSON object, or nil when it is not one.
func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// recordString reads a string field from a JSON object, or "" when absent/not a string.
func recordString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// recordBool reads a boolean field, or false when absent/not a boolean.
func recordBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	return false
}

// recordNumber reads a numeric field as float64, or (0, false) when absent/not a number.
func recordNumber(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	if f, ok := m[key].(float64); ok {
		return f, true
	}
	return 0, false
}

// jsNumber converts a JSON-decoded value the way JavaScript's Number() does.
// Returns NaN (ok=false) for values Number() rejects.
func jsNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		return jsParseNumber(t)
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	default:
		return 0, false
	}
}

// jsParseNumber parses a string the way JavaScript's Number() does:
// surrounding whitespace trimmed, then a decimal (or hex/infinity) literal;
// anything else is NaN. Only the forms the suite can plausibly send are
// supported exactly; the rest falls back to strconv.
func jsParseNumber(s string) (float64, bool) {
	// JavaScript's WhiteSpace + LineTerminator set, as \u escapes.
	t := strings.Trim(s, " \t\n\r\v\f\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
	if t == "" {
		return 0, true
	}
	if t == "Infinity" || t == "+Infinity" {
		return math.Inf(1), true
	}
	if t == "-Infinity" {
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		// Hex integer literals ("0x10") are valid in JS Number().
		if len(t) > 2 && t[0] == '0' && (t[1] == 'x' || t[1] == 'X') {
			if n, err2 := strconv.ParseInt(t[2:], 16, 64); err2 == nil {
				return float64(n), true
			}
		}
		return 0, false
	}
	return f, true
}

// jsParseInt parses a string the way JavaScript's parseInt(s, 10) does:
// leading whitespace skipped, optional sign, then leading decimal digits.
// Returns ok=false when there are no digits (NaN).
func jsParseInt(s string) (int, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	n := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// isIntegerFloat reports whether f is a finite integer (JS Number.isInteger).
func isIntegerFloat(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
}
