package prompton

import "strings"

// StopKind is PromptOn's normalised reason a generation stopped. The provider's
// own finish_reason vocabulary is recorded alongside it, unchanged.
type StopKind string

// The five stop kinds. Anything a provider reports that is not in the table
// below normalises to StopOther.
const (
	StopStop          StopKind = "stop"
	StopLength        StopKind = "length"
	StopToolCall      StopKind = "tool_call"
	StopContentFilter StopKind = "content_filter"
	StopOther         StopKind = "other"
)

// NormalizeStopKind maps a provider finish_reason onto a StopKind. Comparison
// lowercases and trims, so Google's "STOP" and "MAX_TOKENS" land correctly, and
// the mapping is idempotent: feeding a StopKind back in returns itself, which
// matters because the server re-normalises whatever the client sent.
//
// Two traps worth repeating: Google's SAFETY and RECITATION map to
// StopContentFilter's neighbour StopOther (only the literal string
// "content_filter" is a content filter), and a tool call is not a truncation.
func NormalizeStopKind(finishReason string) StopKind {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "stop", "end_turn", "stop_sequence":
		return StopStop
	case "length", "max_tokens":
		return StopLength
	case "tool_call", "tool_calls", "tool_use":
		return StopToolCall
	case "content_filter":
		return StopContentFilter
	default:
		return StopOther
	}
}

// Truncated reports whether the output was cut off. Only StopLength counts —
// the truncation rate, the evaluators and the alerts all share this definition.
func (s StopKind) Truncated() bool { return s == StopLength }

// TruncatedFinishReason is Truncated for a raw provider finish_reason.
func TruncatedFinishReason(finishReason string) bool {
	return NormalizeStopKind(finishReason) == StopLength
}
