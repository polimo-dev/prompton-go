package prompton

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Canonical JSON is the byte representation every PromptOn SDK agrees on: object
// keys sorted byte-wise, no whitespace, no HTML escaping. The monitoring-log
// truncation arithmetic measures sizes on it and hashes it, so two SDKs that
// disagree here would produce different digests for the same record.

// decodeJSON unmarshals into any, keeping numbers as json.Number so that an
// integer stays an integer and 2.0 keeps its decimal point.
func decodeJSON(data []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// decodeJSONValue decodes an arbitrary JSON document into any/json.Number form.
func decodeJSONValue(data []byte) (interface{}, error) {
	var v interface{}
	if err := decodeJSON(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// canonicalJSON encodes v as canonical JSON.
func canonicalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	writeCanonical(&buf, v)
	return buf.Bytes()
}

// jsonSize is the byte length of the canonical JSON encoding of v.
func jsonSize(v interface{}) int {
	return len(canonicalJSON(v))
}

func writeCanonical(buf *bytes.Buffer, v interface{}) {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeJSONString(buf, x)
	case json.Number:
		s := string(x)
		if s == "" {
			buf.WriteString("null")
			return
		}
		buf.WriteString(s)
	case json.RawMessage:
		// Re-canonicalise so raw fragments cannot smuggle in a different shape.
		if decoded, err := decodeJSONValue(x); err == nil {
			writeCanonical(buf, decoded)
		} else {
			buf.WriteString("null")
		}
	case int:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int8:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int16:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint8:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint16:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case float32:
		buf.WriteString(formatFloat(float64(x)))
	case float64:
		buf.WriteString(formatFloat(x))
	case []interface{}:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, el)
		}
		buf.WriteByte(']')
	case []string:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, el)
		}
		buf.WriteByte(']')
	case []map[string]interface{}:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, el)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, x[k])
		}
		buf.WriteByte('}')
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			writeJSONString(buf, x[k])
		}
		buf.WriteByte('}')
	default:
		// Anything else goes through encoding/json once and comes back as a
		// plain value, so struct types still encode canonically.
		b, err := json.Marshal(x)
		if err != nil {
			buf.WriteString("null")
			return
		}
		decoded, err := decodeJSONValue(b)
		if err != nil {
			buf.WriteString("null")
			return
		}
		writeCanonical(buf, decoded)
	}
}

// writeJSONString writes s as a JSON string. A byte that is not part of a valid
// UTF-8 sequence — a provider's truncated multi-byte character, a mangled tool
// argument — becomes U+FFFD rather than travelling raw: the server parses the
// whole request body as UTF-8, so one poisoned byte would otherwise turn into a
// 400 that destroys the entire batch instead of the single record the contract
// says should be rejected.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '"':
				buf.WriteString(`\"`)
			case '\\':
				buf.WriteString(`\\`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			default:
				if c < 0x20 {
					const hex = "0123456789abcdef"
					buf.WriteString(`\u00`)
					buf.WriteByte(hex[c>>4])
					buf.WriteByte(hex[c&0xF])
				} else {
					buf.WriteByte(c)
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString(string(utf8.RuneError))
			i++
			continue
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
}

// formatFloat renders a float64 for canonical JSON. An integral value is
// written as an integer: Go loses the difference between 2 and 2.0 the moment a
// number becomes a float64, and integers are what apps actually send. Values
// decoded by this SDK keep their literal form as json.Number and never reach
// here.
func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		exp = strings.TrimPrefix(exp, "+")
		return mant + "e" + exp
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
