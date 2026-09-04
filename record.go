package prompton

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// A monitoring log is what PromptOn learns about a call it was never in the
// path of. Send failures as well as successes: error rates and truncation rates
// are meaningless without them.

// Error kinds a failed provider call is classified under.
const (
	ErrorKindHTTP4xx     = "http_4xx"
	ErrorKindHTTP5xx     = "http_5xx"
	ErrorKindRateLimited = "rate_limited"
	ErrorKindTimeout     = "timeout"
	ErrorKindTransport   = "transport"
	ErrorKindParse       = "parse"
	ErrorKindApp         = "app"
)

var errorKinds = map[string]bool{
	ErrorKindHTTP4xx: true, ErrorKindHTTP5xx: true, ErrorKindRateLimited: true,
	ErrorKindTimeout: true, ErrorKindTransport: true, ErrorKindParse: true, ErrorKindApp: true,
}

// Cost sources.
const (
	CostSourceProvider = "provider"
	CostSourceCatalog  = "catalog"
	CostSourceUnknown  = "unknown"
)

// CallError describes why a provider call failed. Return one from the function
// you hand to WithGeneration and the kind, status and message are logged as
// they are; any other error is classified for you.
type CallError struct {
	Kind    string
	Status  int
	Message string
}

func (e *CallError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (%d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// NewCallError builds a CallError with the kind normalised.
func NewCallError(kind string, status int, message string) *CallError {
	if !errorKinds[kind] {
		kind = ErrorKindApp
	}
	return &CallError{Kind: kind, Status: status, Message: message}
}

// ErrorKindForStatus maps an HTTP status from a provider onto an error kind.
func ErrorKindForStatus(status int) string {
	switch {
	case status == 429:
		return ErrorKindRateLimited
	case status >= 500:
		return ErrorKindHTTP5xx
	case status >= 400:
		return ErrorKindHTTP4xx
	default:
		return ErrorKindApp
	}
}

// classifyError turns any error into a CallError.
func classifyError(err error) *CallError {
	var ce *CallError
	if errors.As(err, &ce) {
		if !errorKinds[ce.Kind] {
			return NewCallError(ErrorKindApp, ce.Status, ce.Message)
		}
		return ce
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &CallError{Kind: ErrorKindTimeout, Message: err.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &CallError{Kind: ErrorKindTimeout, Message: err.Error()}
	}
	return &CallError{Kind: ErrorKindApp, Message: err.Error()}
}

// Usage is what the call cost. Pointer fields distinguish "not reported" from
// zero, which is the difference between a free call and an unknown one.
type Usage struct {
	InputTokens  *int
	OutputTokens *int
	CostUSD      *float64
	// CostSource is provider, catalog or unknown.
	CostSource string
	// Raw is the provider's own usage object, kept for auditing. Over 16 KB the
	// server blanks it rather than rejecting the record.
	Raw map[string]interface{}
}

// Input is what went into the provider call.
type Input struct {
	// Variables are the values the prompt was rendered with.
	Variables map[string]interface{}
	// Messages is the final message list actually sent, after the app attached
	// any history of its own.
	Messages []Message
	// Text is the single prompt string for a text call.
	Text string
}

// Output is what came back.
type Output struct {
	Content   string
	ToolCalls []interface{}
}

// SDKInfo names the client that sent a record.
type SDKInfo struct {
	Name    string
	Version string
}

// GenerationRecord is one monitoring log: one model call the app made.
//
// Required: UseCase, Model, Status and StartedAt. ID is filled with a fresh
// UUIDv7 when empty — PromptOn stores generation ids in a UUIDv7 column, so a
// v4 id passes validation and then fails on write.
type GenerationRecord struct {
	ID      string
	UseCase string
	Kind    Kind
	Model   string
	Status  string

	StartedAt time.Time
	LatencyMS int

	// latencyMeasured distinguishes "the call took under a millisecond" from
	// "nobody timed it", so WithGeneration always reports a latency and a
	// hand-built record only reports one it actually has.
	latencyMeasured bool

	// Resolution, when set, fills the deployment, prompt, model and source
	// fields this record does not carry itself.
	Resolution *Resolution

	DeploymentID       string
	DeploymentRevision int
	Prompt             string
	PromptVersionID    string
	ModelID            string
	ResolutionSource   Source

	Provider         string
	ModelUsed        string
	UpstreamProvider string

	Params map[string]interface{}
	Input  *Input
	Output *Output

	FinishReason string
	StopKind     StopKind
	Error        *CallError
	Usage        *Usage

	TraceID    string
	Sequence   int
	EndUserRef string
	Context    map[string]interface{}
	Metadata   map[string]interface{}

	// Environment overrides the client's environment for this record. Batches
	// are sent per environment, so records with different environments never
	// share a request.
	Environment string

	// SDK names the client. Filled in when empty.
	SDK *SDKInfo
}

// Statuses a record can carry.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// applyResolution fills the resolution evidence from r for every field the
// caller left empty.
func (rec *GenerationRecord) applyResolution() {
	r := rec.Resolution
	if r == nil {
		return
	}
	if rec.UseCase == "" {
		rec.UseCase = r.UseCase
	}
	if rec.Kind == "" {
		rec.Kind = r.Kind
	}
	if rec.Model == "" {
		rec.Model = r.Model
	}
	if rec.ModelID == "" {
		rec.ModelID = r.ModelID
	}
	if rec.Provider == "" {
		rec.Provider = r.Provider
	}
	if rec.DeploymentID == "" {
		rec.DeploymentID = r.DeploymentID
	}
	if rec.DeploymentRevision == 0 {
		rec.DeploymentRevision = r.DeploymentRevision
	}
	if rec.Prompt == "" {
		rec.Prompt = r.Prompt
	}
	if rec.PromptVersionID == "" {
		rec.PromptVersionID = r.PromptVersionID
	}
	if rec.ResolutionSource == "" {
		rec.ResolutionSource = r.Source
	}
	if rec.Params == nil {
		rec.Params = r.Params
	}
}

func (rec *GenerationRecord) validate() error {
	if rec.UseCase == "" {
		return errors.New("prompton: monitoring log needs a use_case")
	}
	if rec.Model == "" {
		return errors.New("prompton: monitoring log needs a model")
	}
	if rec.Status != StatusOK && rec.Status != StatusError {
		return fmt.Errorf("prompton: monitoring log status must be %q or %q, got %q", StatusOK, StatusError, rec.Status)
	}
	if rec.StartedAt.IsZero() {
		return errors.New("prompton: monitoring log needs a started_at")
	}
	return nil
}

// startedAtWindow is what the server accepts: at most 5 minutes in the future
// and 7 days in the past. Catching it here turns a silent per-record rejection
// into an error the caller can see.
func (rec *GenerationRecord) checkStartedAt(now time.Time) error {
	if rec.StartedAt.After(now.Add(5 * time.Minute)) {
		return fmt.Errorf("prompton: started_at %s is more than 5 minutes in the future", rec.StartedAt.UTC().Format(time.RFC3339))
	}
	if rec.StartedAt.Before(now.Add(-7 * 24 * time.Hour)) {
		return fmt.Errorf("prompton: started_at %s is more than 7 days in the past", rec.StartedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// toMap renders the record in the wire shape POST /generations accepts. A
// top-level key whose value is null is omitted; nested nulls inside usage are
// sent and accepted.
func (rec *GenerationRecord) toMap() map[string]interface{} {
	out := map[string]interface{}{
		"id":         rec.ID,
		"use_case":   rec.UseCase,
		"model":      rec.Model,
		"status":     rec.Status,
		"started_at": rec.StartedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
	}
	putString(out, "kind", string(rec.Kind))
	putString(out, "deployment_id", rec.DeploymentID)
	if rec.DeploymentRevision != 0 {
		out["deployment_revision"] = rec.DeploymentRevision
	}
	putString(out, "prompt", rec.Prompt)
	putString(out, "prompt_version_id", rec.PromptVersionID)
	putString(out, "model_id", rec.ModelID)
	putString(out, "resolution_source", string(rec.ResolutionSource))
	putString(out, "provider", rec.Provider)
	putString(out, "model_used", rec.ModelUsed)
	putString(out, "upstream_provider", rec.UpstreamProvider)
	putString(out, "finish_reason", rec.FinishReason)
	putString(out, "stop_kind", string(rec.StopKind))
	putString(out, "trace_id", rec.TraceID)
	putString(out, "end_user_ref", rec.EndUserRef)
	if rec.Sequence != 0 {
		out["sequence"] = rec.Sequence
	}
	if rec.LatencyMS != 0 || rec.latencyMeasured {
		out["latency_ms"] = rec.LatencyMS
	}
	if len(rec.Params) > 0 {
		out["params"] = toPlainMap(rec.Params)
	}
	if rec.Context != nil {
		out["context"] = toPlainMap(rec.Context)
	}
	if rec.Metadata != nil {
		out["metadata"] = toPlainMap(rec.Metadata)
	}
	if in := rec.Input.toMap(); in != nil {
		out["input"] = in
	}
	if o := rec.Output.toMap(); o != nil {
		out["output"] = o
	}
	if rec.Error != nil {
		errMap := map[string]interface{}{"kind": rec.Error.Kind}
		if rec.Error.Status != 0 {
			errMap["status"] = rec.Error.Status
		}
		if rec.Error.Message != "" {
			errMap["message"] = rec.Error.Message
		}
		out["error"] = errMap
	}
	if rec.Usage != nil {
		out["usage"] = rec.Usage.toMap()
	}
	sdk := rec.SDK
	if sdk == nil {
		sdk = &SDKInfo{Name: SDKName, Version: Version}
	}
	out["sdk"] = map[string]interface{}{"name": sdk.Name, "version": sdk.Version}
	return out
}

func (u *Usage) toMap() map[string]interface{} {
	source := u.CostSource
	if source == "" {
		source = CostSourceUnknown
	}
	out := map[string]interface{}{"cost_source": source}
	if u.InputTokens != nil {
		out["input_tokens"] = *u.InputTokens
	} else {
		out["input_tokens"] = nil
	}
	if u.OutputTokens != nil {
		out["output_tokens"] = *u.OutputTokens
	} else {
		out["output_tokens"] = nil
	}
	if u.CostUSD != nil {
		out["cost_usd"] = *u.CostUSD
	} else {
		out["cost_usd"] = nil
	}
	if u.Raw != nil {
		out["raw"] = toPlainMap(u.Raw)
	} else {
		out["raw"] = nil
	}
	return out
}

func (in *Input) toMap() map[string]interface{} {
	if in == nil {
		return nil
	}
	out := map[string]interface{}{}
	if in.Variables != nil {
		out["variables"] = toPlainMap(in.Variables)
	}
	if in.Messages != nil {
		out["messages"] = messagesToList(in.Messages)
	}
	if in.Text != "" {
		out["text"] = in.Text
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (o *Output) toMap() map[string]interface{} {
	if o == nil {
		return nil
	}
	out := map[string]interface{}{}
	if o.Content != "" {
		out["content"] = o.Content
	}
	if o.ToolCalls != nil {
		out["tool_calls"] = o.ToolCalls
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func messagesToList(messages []Message) []interface{} {
	out := make([]interface{}, len(messages))
	for i, m := range messages {
		entry := map[string]interface{}{"role": m.Role, "content": m.Content}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		out[i] = entry
	}
	return out
}

func putString(m map[string]interface{}, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// toPlainMap makes a defensive copy so a record queued for sending cannot
// change under the caller's feet.
func toPlainMap(m map[string]interface{}) map[string]interface{} {
	return deepCopyMap(m)
}

// IntPtr is a convenience for the pointer fields of Usage.
func IntPtr(v int) *int { return &v }

// Float64Ptr is a convenience for the pointer fields of Usage.
func Float64Ptr(v float64) *float64 { return &v }
