// Package liquid implements the Liquid subset PromptOn allows in prompt
// templates: {{ output }}, the for/if/unless/assign tags with break and
// continue, and the size, join and default filters. Nothing else parses.
//
// It is deliberately small. The server rejects a template outside this subset
// when the prompt version is committed, so a template that fails Lint can never
// reach a snapshot, and a renderer that only knows this much can still render
// everything PromptOn will ever hand it.
package liquid

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Normalize converts a Go value into the shapes the renderer understands:
// map[string]interface{}, []interface{}, string, bool, json.Number and nil.
// Struct and pointer values round-trip through encoding/json.
func Normalize(v interface{}) interface{} {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, json.Number:
		return x
	case int:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int8:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int16:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int32:
		return json.Number(strconv.FormatInt(int64(x), 10))
	case int64:
		return json.Number(strconv.FormatInt(x, 10))
	case uint:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint8:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint16:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint32:
		return json.Number(strconv.FormatUint(uint64(x), 10))
	case uint64:
		return json.Number(strconv.FormatUint(x, 10))
	case float32:
		return json.Number(FormatFloat(float64(x)))
	case float64:
		return json.Number(FormatFloat(x))
	case json.RawMessage:
		return Normalize(string(x))
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, el := range x {
			out[i] = Normalize(el)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, el := range x {
			out[k] = Normalize(el)
		}
		return out
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = Normalize(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]interface{}, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = Normalize(iter.Value().Interface())
		}
		return out
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return Normalize(rv.Elem().Interface())
	}

	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var decoded interface{}
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Sprint(v)
	}
	return decoded
}

// NormalizeMap normalises a variables map, tolerating a nil argument.
func NormalizeMap(vars map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(vars))
	for k, v := range vars {
		out[k] = Normalize(v)
	}
	return out
}

// FormatFloat renders a float for output and for canonical JSON.
//
// Go cannot tell 2 from 2.0 once a value is a float64 — the reference
// implementation can, and renders the float as "2.0". Rendering an integral
// float as an integer is the friendlier half of that trade: apps overwhelmingly
// template counts and ids, and an app that decodes its variables through this
// SDK keeps the distinction anyway, because JSON numbers are decoded as
// json.Number and never lose their literal form.
func FormatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0.0"
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
		return mant + "e" + strings.TrimPrefix(exp, "+")
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// ToString renders a value into an output position. Strings pass through with
// no HTML escaping, numbers keep their literal form, nil renders as the empty
// string, and a list renders as its elements concatenated with no separator —
// the Liquid rule, and almost never what you want: use join.
func ToString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return string(x)
	case []interface{}:
		var b strings.Builder
		for _, el := range x {
			b.WriteString(ToString(el))
		}
		return b.String()
	case map[string]interface{}:
		// Rendering a map into an output position is unspecified across SDKs.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%q => %s", k, ToString(x[k])))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(x)
	}
}

// Truthy follows Liquid: only nil and false are falsy. An empty string, an
// empty list and zero are all truthy.
func Truthy(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	default:
		return true
	}
}

// blank reports the emptiness the default filter reacts to.
func blank(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case []interface{}:
		return len(x) == 0
	case map[string]interface{}:
		return len(x) == 0
	default:
		return false
	}
}

func numberOf(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		return 0, false
	default:
		return 0, false
	}
}

func sizeOf(v interface{}) int {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x)
	case []interface{}:
		return len(x)
	case map[string]interface{}:
		return len(x)
	case nil:
		return 0
	default:
		return 0
	}
}
