package prompton

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// The payload policy decides how much of a model call's input and output ever
// leaves the process. The server re-checks the same rules, but the SDK applies
// them first so the raw text never travels: if the SDK is more conservative,
// the SDK wins.
//
// Order of operations, and it matters because the steps interact:
//
//  1. keep decision (sampling; errors and stop_kind=length are always kept)
//  2. wrap a string input as {"text": …} and a string output as {"content": …}
//  3. apply the mode: none drops, hash digests, full truncates
//  4. cap error.message at 2048 bytes
//  5. hash end_user_ref when hash_end_user is set
//  6. run the redact hook last

const (
	errorMessageMax = 2048
	sampleScale     = 10000
)

type payloadSettings struct {
	Defaults    PayloadPolicy
	HashEndUser bool
	Redact      func(map[string]interface{}) map[string]interface{}
	logf        func(string, ...interface{})
}

// UnmarshalJSON fills the defaults PromptOn documents for a payload policy, so
// a policy that names only a mode still samples everything at 256 KiB.
func (p *PayloadPolicy) UnmarshalJSON(data []byte) error {
	type alias PayloadPolicy
	tmp := alias{Mode: PayloadFull, SampleRate: 1.0, MaxBytes: DefaultMaxBytes}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*p = PayloadPolicy(tmp)
	return nil
}

func normalizePolicy(policy *PayloadPolicy, defaults PayloadPolicy) PayloadPolicy {
	out := defaults
	if out.Mode == "" {
		out.Mode = PayloadFull
	}
	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultMaxBytes
	}
	if policy != nil {
		if policy.Mode != "" {
			out.Mode = policy.Mode
		}
		if policy.MaxBytes > 0 {
			out.MaxBytes = policy.MaxBytes
		}
		out.SampleRate = policy.SampleRate
	}
	switch out.Mode {
	case PayloadFull, PayloadHash, PayloadNone:
	default:
		out.Mode = PayloadFull
	}
	if out.SampleRate < 0 {
		out.SampleRate = 0
	}
	if out.SampleRate > 1 {
		out.SampleRate = 1
	}
	return out
}

// applyPayloadPolicy returns a copy of gen with the policy applied.
func applyPayloadPolicy(gen map[string]interface{}, policy *PayloadPolicy, cfg payloadSettings) map[string]interface{} {
	out := deepCopyMap(gen)
	p := normalizePolicy(policy, cfg.Defaults)

	out = applyPayloadMode(out, p)
	out = capErrorMessage(out)
	if cfg.HashEndUser {
		if ref, ok := out["end_user_ref"]; ok && ref != nil {
			out["end_user_ref"] = sha256Hex([]byte(valueToString(ref)))
		}
	}
	if cfg.Redact != nil {
		out = runRedact(out, cfg)
	}
	return out
}

func runRedact(gen map[string]interface{}, cfg payloadSettings) (result map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			if cfg.logf != nil {
				cfg.logf("redact hook panicked (%v); dropping the payload", r)
			}
			result = dropPayload(gen)
		}
	}()
	redacted := cfg.Redact(gen)
	if redacted == nil {
		if cfg.logf != nil {
			cfg.logf("redact hook returned nil; dropping the payload")
		}
		return dropPayload(gen)
	}
	return redacted
}

func applyPayloadMode(gen map[string]interface{}, p PayloadPolicy) map[string]interface{} {
	if p.Mode == PayloadNone {
		return dropPayload(gen)
	}
	if !keepPayload(gen, p.SampleRate) {
		return dropPayload(gen)
	}
	gen = wrapPayload(gen)
	if p.Mode == PayloadHash {
		return hashPayload(gen)
	}
	return truncatePayload(gen, p.MaxBytes)
}

func dropPayload(gen map[string]interface{}) map[string]interface{} {
	delete(gen, "input")
	delete(gen, "output")
	return gen
}

// keepPayload is the sampling decision. It is a pure function of the record id,
// so a resend makes the same decision and the server reaches the same answer
// independently. An error you cannot see is worse than a storage bill, and a
// truncated answer is the one you most need the text of, so both are always
// kept.
func keepPayload(gen map[string]interface{}, rate float64) bool {
	if valueToString(gen["status"]) == "error" {
		return true
	}
	if valueToString(gen["stop_kind"]) == "length" {
		return true
	}
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	return sampleBucket(valueToString(gen["id"])) < uint32(math.Round(rate*sampleScale))
}

// sampleBucket is the first 4 bytes of sha256(id) as an unsigned big-endian
// 32-bit integer, mod 10000.
func sampleBucket(id string) uint32 {
	sum := sha256.Sum256([]byte(id))
	return binary.BigEndian.Uint32(sum[:4]) % sampleScale
}

// wrapPayload turns a bare string input or output into the object shape the
// server stores, regardless of what happens next.
func wrapPayload(gen map[string]interface{}) map[string]interface{} {
	if s, ok := gen["input"].(string); ok {
		gen["input"] = map[string]interface{}{"text": s}
	}
	if s, ok := gen["output"].(string); ok {
		gen["output"] = map[string]interface{}{"content": s}
	}
	return gen
}

// hashPayload replaces input and output with the pre-hashed wrapper the server
// recognises. The digest covers the canonical JSON of the wrapped value, so a
// string input hashes {"text":"…"} rather than the bare string.
func hashPayload(gen map[string]interface{}) map[string]interface{} {
	for _, key := range []string{"input", "output"} {
		v, ok := gen[key]
		if !ok || v == nil {
			continue
		}
		encoded := canonicalJSON(v)
		gen[key] = map[string]interface{}{
			"sha256": sha256Hex(encoded),
			"bytes":  len(encoded),
			"hashed": true,
		}
	}
	return gen
}

func truncatePayload(gen map[string]interface{}, maxBytes int) map[string]interface{} {
	if v, ok := gen["input"]; ok {
		if out := truncateInput(v, maxBytes); out == nil {
			delete(gen, "input")
		} else {
			gen["input"] = out
		}
	}
	if v, ok := gen["output"]; ok {
		if out := truncateOutput(v, maxBytes); out == nil {
			delete(gen, "output")
		} else {
			gen["output"] = out
		}
	}
	return gen
}

func truncateInput(v interface{}, maxBytes int) interface{} {
	input, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	perMessage := maxInt(maxBytes/8, 64)
	varLimit := maxInt(maxBytes/4, 64)

	truncated := false
	if raw, ok := input["messages"]; ok {
		msgs, cut := truncateMessages(raw, perMessage, maxBytes)
		truncated = truncated || cut
		if msgs != nil {
			input["messages"] = msgs
		}
	}
	if raw, ok := input["text"]; ok {
		if s, isStr := raw.(string); isStr {
			out, cut := truncateString(s, maxBytes)
			truncated = truncated || cut
			input["text"] = out
		}
	}
	if raw, ok := input["variables"]; ok && raw != nil {
		vars, cut := truncateVariables(raw, varLimit)
		truncated = truncated || cut
		input["variables"] = vars
	}
	if truncated {
		input["truncated"] = true
	}
	return input
}

func truncateOutput(v interface{}, maxBytes int) interface{} {
	output, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	limit := maxInt(maxBytes/4, 64)
	truncated := false
	if raw, ok := output["content"]; ok {
		if s, isStr := raw.(string); isStr {
			out, cut := truncateString(s, limit)
			truncated = truncated || cut
			output["content"] = out
		}
	}
	if raw, ok := output["tool_calls"]; ok && raw != nil {
		calls, cut := truncateToolCalls(raw, limit)
		truncated = truncated || cut
		output["tool_calls"] = calls
	}
	if truncated {
		output["truncated"] = true
	}
	return output
}

func truncateVariables(v interface{}, limit int) (interface{}, bool) {
	encoded := canonicalJSON(v)
	if len(encoded) <= limit {
		return v, false
	}
	// Variables are never partially cut: half a JSON object is worse than a
	// digest that says how much there was.
	return map[string]interface{}{
		"truncated": true,
		"sha256":    sha256Hex(encoded),
		"bytes":     len(encoded),
	}, true
}

func truncateMessages(v interface{}, perMessage, totalLimit int) (interface{}, bool) {
	list, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	truncated := false
	out := make([]interface{}, len(list))
	for i, m := range list {
		msg, cut := truncateMessage(m, perMessage)
		truncated = truncated || cut
		out[i] = msg
	}
	if listJSONSize(out) <= totalLimit {
		return out, truncated
	}
	return fitMessages(out, totalLimit), true
}

func truncateMessage(v interface{}, limit int) (interface{}, bool) {
	msg, ok := v.(map[string]interface{})
	if !ok {
		return v, false
	}
	content, exists := msg["content"]
	if !exists || content == nil {
		return msg, false
	}
	if s, isStr := content.(string); isStr {
		out, cut := truncateString(s, limit)
		msg["content"] = out
		if cut {
			msg["truncated"] = true
		}
		return msg, cut
	}
	encoded := canonicalJSON(content)
	if len(encoded) <= limit {
		return msg, false
	}
	out, _ := truncateString(string(encoded), limit)
	msg["content"] = out
	msg["truncated"] = true
	return msg, true
}

// fitMessages brings a whole message list under the total cap. First the middle
// messages are emptied into byte-count stubs from the front — the first message
// (the system prompt) and the last (the newest turn) are always preserved, so a
// later middle message can survive intact. If stubbing is not enough the middle
// is dropped entirely and replaced by one marker message.
func fitMessages(messages []interface{}, limit int) []interface{} {
	stubbed := stubMiddle(messages, limit)
	if listJSONSize(stubbed) <= limit {
		return stubbed
	}
	return dropMiddle(messages, limit)
}

func stubMiddle(messages []interface{}, limit int) []interface{} {
	count := len(messages)
	running := listJSONSize(messages)
	out := make([]interface{}, count)
	copy(out, messages)
	for i := 1; i < count-1; i++ {
		if running <= limit {
			break
		}
		msg, ok := out[i].(map[string]interface{})
		if !ok {
			continue
		}
		before := jsonSize(msg)
		stub := deepCopyMap(msg)
		stub["content"] = fmt.Sprintf("…[truncated %d bytes]…", messageContentBytes(msg))
		stub["truncated"] = true
		out[i] = stub
		running = running - before + jsonSize(stub)
	}
	return out
}

func dropMiddle(messages []interface{}, limit int) []interface{} {
	if len(messages) == 0 {
		return messages
	}
	first := messages[0]
	rest := messages[1:]
	for {
		marker := map[string]interface{}{
			"role":      "system",
			"content":   markerText(len(rest)),
			"truncated": true,
		}
		base := listJSONSize([]interface{}{first, marker})
		if base <= limit {
			kept := tailWithin(rest, limit-base)
			marker["content"] = markerText(len(rest) - len(kept))
			out := make([]interface{}, 0, len(kept)+2)
			out = append(out, first, marker)
			out = append(out, kept...)
			return out
		}
		smaller, shrunk := shrinkFirst(first)
		if !shrunk {
			marker["content"] = markerText(len(rest) + 1)
			if listJSONSize([]interface{}{marker}) <= limit {
				return []interface{}{marker}
			}
			return []interface{}{}
		}
		first = smaller
	}
}

// shrinkFirst makes the preserved first message smaller: halve its content, or
// strip it down to the role once there is no content left. The second return
// value is false when there is nothing left to shrink, which is what stops
// dropMiddle from looping.
func shrinkFirst(v interface{}) (interface{}, bool) {
	msg, ok := v.(map[string]interface{})
	if !ok {
		return v, false
	}
	if messageContentBytes(msg) == 0 {
		minimal := map[string]interface{}{"truncated": true}
		if role, ok := msg["role"]; ok {
			minimal["role"] = role
		}
		if string(canonicalJSON(minimal)) == string(canonicalJSON(msg)) {
			return v, false
		}
		return minimal, true
	}
	shrunk, _ := truncateMessage(deepCopyMap(msg), messageContentBytes(msg)/2)
	return shrunk, true
}

func tailWithin(messages []interface{}, budget int) []interface{} {
	var kept []interface{}
	left := budget
	for i := len(messages) - 1; i >= 0; i-- {
		size := jsonSize(messages[i]) + 1
		if size > left {
			break
		}
		left -= size
		kept = append([]interface{}{messages[i]}, kept...)
	}
	return kept
}

func markerText(dropped int) string {
	return fmt.Sprintf("…[%d messages truncated]…", dropped)
}

func messageContentBytes(msg map[string]interface{}) int {
	content, ok := msg["content"]
	if !ok || content == nil {
		return 0
	}
	if s, isStr := content.(string); isStr {
		return len(s)
	}
	return jsonSize(content)
}

// listJSONSize is the canonical JSON size of a list: one byte per bracket plus
// the elements and their separating commas.
func listJSONSize(list []interface{}) int {
	if len(list) == 0 {
		return 2
	}
	size := 1
	for _, el := range list {
		size += jsonSize(el) + 1
	}
	return size
}

func truncateToolCalls(v interface{}, limit int) (interface{}, bool) {
	calls, ok := v.([]interface{})
	if !ok {
		return v, false
	}
	if jsonSize(calls) <= limit {
		return calls, false
	}
	stripped := make([]interface{}, len(calls))
	for i, c := range calls {
		stripped[i] = putArguments(c, "")
	}
	overhead := jsonSize(stripped)
	budget := maxInt(limit-overhead, 0) / maxInt(len(calls), 1)
	for budget >= 32 {
		shrunk := make([]interface{}, len(calls))
		for i, c := range calls {
			shrunk[i] = shrinkCall(c, budget)
		}
		if jsonSize(shrunk) <= limit {
			return shrunk, true
		}
		budget /= 2
	}
	return []interface{}{map[string]interface{}{"truncated": true, "bytes": jsonSize(calls)}}, true
}

func shrinkCall(call interface{}, budget int) interface{} {
	m, ok := call.(map[string]interface{})
	if !ok {
		return call
	}
	fn, ok := m["function"].(map[string]interface{})
	if !ok {
		return call
	}
	args, ok := fn["arguments"].(string)
	if !ok {
		return call
	}
	shrunk, _ := truncateString(args, budget)
	return putArguments(m, shrunk)
}

func putArguments(call interface{}, args string) interface{} {
	m, ok := call.(map[string]interface{})
	if !ok {
		return call
	}
	fn, ok := m["function"].(map[string]interface{})
	if !ok {
		return call
	}
	if _, isStr := fn["arguments"].(string); !isStr {
		return call
	}
	newFn := deepCopyMap(fn)
	newFn["arguments"] = args
	newCall := deepCopyMap(m)
	newCall["function"] = newFn
	return newCall
}

// truncateString cuts a string down to limit bytes, keeping the head and the
// tail and losing the middle. The budget left after the marker is split 60%
// head / 40% tail, then each side is trimmed back to a UTF-8 boundary, so the
// result is never longer than the cap and never splits a character.
func truncateString(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	marker := fmt.Sprintf("\n…[truncated %d bytes]…\n", len(s)-limit)
	if len(marker) > limit {
		// A cap too small even for the marker: keep the head only.
		return trimTrailingPartial(s[:limit]), true
	}
	budget := limit - len(marker)
	headLen := budget * 6 / 10
	tailLen := budget - headLen
	head := trimTrailingPartial(s[:headLen])
	tail := trimLeadingPartial(s[len(s)-tailLen:])
	return head + marker + tail, true
}

func trimTrailingPartial(s string) string {
	for tries := 0; tries < 3 && len(s) > 0; tries++ {
		if utf8.ValidString(s) {
			return s
		}
		s = s[:len(s)-1]
	}
	if utf8.ValidString(s) {
		return s
	}
	return ""
}

func trimLeadingPartial(s string) string {
	for len(s) > 0 && s[0] >= 0x80 && s[0] < 0xC0 {
		s = s[1:]
	}
	return s
}

func capErrorMessage(gen map[string]interface{}) map[string]interface{} {
	errObj, ok := gen["error"].(map[string]interface{})
	if !ok {
		return gen
	}
	msg, ok := errObj["message"].(string)
	if !ok || len(msg) <= errorMessageMax {
		return gen
	}
	capped, _ := truncateString(msg, errorMessageMax)
	errObj["message"] = capped
	return gen
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func valueToString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return formatFloat(x)
	default:
		return fmt.Sprint(x)
	}
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(x)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, el := range x {
			out[i] = deepCopyValue(el)
		}
		return out
	default:
		return v
	}
}
