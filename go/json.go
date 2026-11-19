// Fast, allocation-light JSON writing for the hot REST views, plus the small
// JavaScript-number helpers the port needs to match the TypeScript semantics
// (JSON.parse/Number/parseInt) where the suite can observe the difference.

package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
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
