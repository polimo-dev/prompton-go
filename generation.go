package prompton

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The three ways a monitoring log reaches PromptOn:
//
//	Log             — you built the record; it is queued and the call returns
//	Flush           — send the queue now and wait (shutdown, tests, scripts)
//	Track  — time your provider call and log it for you
//
// None of them ever blocks the provider call.

// CallMeta is what Track cannot work out for itself: what went in, who
// it was for, and how to correlate it.
type CallMeta struct {
	// ID pre-issues the record id, for an app that wants to store it alongside
	// its own row before the call. Empty means a fresh UUIDv7.
	ID string

	// Variables are the values the prompt was rendered with. Render is pure and
	// keeps no state, so pass the same map here if you want them logged.
	Variables map[string]interface{}

	// Messages is the final message list sent to the provider, after any
	// history the app attached. Text is its single-string equivalent.
	Messages []Message
	Text     string

	EndUserRef string
	TraceID    string
	Sequence   int

	// Context is a free-form tag map (language, plan, whatever you slice by).
	// useCaseResolution does not look at it: it is log-only. At most 2 KB or the
	// server rejects the record.
	Context map[string]interface{}

	// Metadata is free app data. At most 4 KB or the server rejects the record.
	Metadata map[string]interface{}

	// Params overrides the resolution's parameters with what was actually sent.
	Params map[string]interface{}

	// Environment overrides the client's environment for this record.
	Environment string
}

// Result is what your provider call produced. Return one from the function you
// give Track; return it alongside an error too, when the provider
// answered but the app could not use the answer — the call still counts as
// spend and as a quality signal.
type Result struct {
	Content   string
	ToolCalls []interface{}

	// FinishReason is the provider's raw value. StopKind is derived from it
	// when left empty.
	FinishReason string
	StopKind     StopKind

	Usage *Usage

	// ModelUsed and UpstreamProvider record what actually served the call when
	// a router picked something other than what was asked for.
	ModelUsed        string
	UpstreamProvider string

	// IsByok records that the call used the app's own provider key, stored
	// under metadata.
	IsByok *bool

	// Result is yours: Track returns the Result unchanged, so this is
	// a convenient place to carry the parsed answer back to the caller.
	Result interface{}
}

// Log queues one monitoring log and returns immediately. It returns ErrClosed
// when the client has already been closed, and a validation error when the
// record is missing a required field; every other failure mode — a full queue,
// an oversized record, a server that refuses the batch — is counted in
// BufferStats rather than returned, because a monitoring log must never become
// the caller's problem.
//
// It fills in the id (a UUIDv7), the started_at, the sdk name and version, and —
// when the record carries UseCaseEvidence — the deployment, prompt, model and
// source fields. The payload policy of the use case is applied
// before the record is queued, so raw text never travels further than the
// policy allows.
func (c *Client) Log(rec LogRecord) error {
	rec.applyResolution()
	if rec.ID == "" {
		rec.ID = NewLogID()
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = c.cfg.now()
	}
	if rec.Status == "" {
		if rec.Error != nil {
			rec.Status = StatusError
		} else {
			rec.Status = StatusOK
		}
	}
	if rec.StopKind == "" && rec.FinishReason != "" {
		rec.StopKind = NormalizeStopKind(rec.FinishReason)
	} else if rec.StopKind != "" {
		rec.StopKind = NormalizeStopKind(string(rec.StopKind))
	}
	if err := rec.validate(); err != nil {
		return err
	}
	if err := rec.checkStartedAt(c.cfg.now()); err != nil {
		return err
	}

	environment := rec.Environment
	if environment == "" {
		environment = c.cfg.Environment
	}

	payload := applyPayloadPolicy(rec.toMap(), c.policyFor(&rec), payloadSettings{
		Defaults:    c.cfg.PayloadDefaults,
		HashEndUser: c.cfg.HashEndUser,
		Redact:      c.cfg.Redact,
		logf:        c.cfg.Logger,
	})

	if c.buffer == nil {
		if c.closed.Load() {
			// The same answer the buffered path gives: a closed client says so
			// instead of accepting a record nobody will ever read.
			c.recordedMu.Lock()
			c.recordedClosed++
			c.recordedMu.Unlock()
			c.warnOnce("log-closed", "dropping a monitoring log: the client is closed")
			return ErrClosed
		}
		// Test and offline modes, and a live client with no API key: keep the
		// record where Recorded can read it rather than pretending to send it.
		// The capture is bounded exactly like the send queue — a process that
		// boots without an API key must ride the misconfiguration out, not grow
		// until it is killed.
		c.recordedMu.Lock()
		c.recorded = append(c.recorded, payload)
		dropped := 0
		if limit := c.cfg.LogMaxBuffer; limit > 0 && len(c.recorded) > limit {
			dropped = len(c.recorded) - limit
			c.recorded = append(c.recorded[:0], c.recorded[dropped:]...)
			c.recordedDropped += dropped
		}
		c.recordedMu.Unlock()
		if dropped > 0 {
			c.warnOnce("log-overflow", "monitoring-log queue full: dropped %d oldest record(s)", dropped)
		}
		return nil
	}
	return c.buffer.enqueue(environment, payload)
}

// TraceEvent is one monitored step inside a trace: a tool attempt or a final completion.
// The SDK never calls customer tools; it only submits the events your app observed.
type TraceEvent map[string]interface{}

const (
	EventKindToolAttempt = "tool_attempt"
	EventKindCompletion  = "completion"

	EventStatusStarted    = "started"
	EventStatusOK         = "ok"
	EventStatusError      = "error"
	EventStatusDenied     = "denied"
	EventStatusCancelled  = "cancelled"
	EventStatusTimeout    = "timeout"
	EventStatusMissing    = "missing"
	EventStatusIncomplete = "incomplete"
)

var eventKinds = map[string]bool{
	EventKindToolAttempt: true,
	EventKindCompletion:  true,
}

var eventStatuses = map[string]bool{
	EventStatusStarted:    true,
	EventStatusOK:         true,
	EventStatusError:      true,
	EventStatusDenied:     true,
	EventStatusCancelled:  true,
	EventStatusTimeout:    true,
	EventStatusMissing:    true,
	EventStatusIncomplete: true,
}

// LogEvents submits observed tool/completion trace events synchronously. It fills event_id,
// observed_at and sdk when absent, preserving generated IDs on the supplied maps so a
// caller retrying the same slice sends the same event ids.
func (c *Client) LogEvents(ctx context.Context, events []TraceEvent, environment ...string) (*BatchResult, error) {
	if len(events) == 0 {
		return &BatchResult{}, nil
	}
	if len(events) > 500 {
		return nil, errors.New("prompton: LogEvents accepts at most 500 events")
	}
	env := c.cfg.Environment
	if len(environment) > 0 && environment[0] != "" {
		env = environment[0]
	}
	prepared := make([]map[string]interface{}, len(events))
	for i := range events {
		if events[i] == nil {
			return nil, fmt.Errorf("prompton: trace event %d must be an object", i)
		}
		prepared[i] = events[i]
		if _, ok := events[i]["event_id"].(string); !ok || events[i]["event_id"] == "" {
			events[i]["event_id"] = NewLogID()
		}
		if _, ok := events[i]["observed_at"].(string); !ok || events[i]["observed_at"] == "" {
			events[i]["observed_at"] = c.cfg.now().UTC().Format("2006-01-02T15:04:05.000000Z")
		}
		if traceID, ok := events[i]["trace_id"].(string); !ok || traceID == "" {
			return nil, fmt.Errorf("prompton: trace event %d needs trace_id", i)
		}
		kind, ok := events[i]["event_kind"].(string)
		if !ok || !eventKinds[kind] {
			return nil, fmt.Errorf("prompton: trace event %d has unsupported event_kind", i)
		}
		status, ok := events[i]["status"].(string)
		if !ok || !eventStatuses[status] {
			return nil, fmt.Errorf("prompton: trace event %d has unsupported status", i)
		}
		ensureEventSDK(events[i])
	}
	if c.cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	return c.postEvents(ctx, env, prepared)
}

func ensureEventSDK(event map[string]interface{}) {
	if _, ok := event["sdk"]; !ok {
		event["sdk"] = map[string]interface{}{"name": SDKName, "version": Version}
	}
}

// policyFor is the use case's payload policy: the one carried by the use-case
// evidence if there is one, else the document's, else the client defaults.
func (c *Client) policyFor(rec *LogRecord) *PayloadPolicy {
	evidence := rec.useCaseResolution
	if evidence == nil && rec.UseCaseEvidence != nil {
		evidence = rec.UseCaseEvidence.useCaseResolution
	}
	if evidence != nil && evidence.PayloadPolicy != nil {
		return evidence.PayloadPolicy
	}
	entry := c.store.get(rec.UseCase)
	if entry == nil {
		return nil
	}
	if uc, ok := entry.snapshot.UseCases[rec.UseCase]; ok {
		return uc.PayloadPolicy
	}
	return nil
}

// Flush sends everything queued and waits for the result. Call it at shutdown,
// in tests, and at the end of a script.
func (c *Client) Flush(ctx context.Context) error {
	if c.buffer == nil {
		return nil
	}
	return c.buffer.flush(ctx)
}

// BufferStats reports what the monitoring-log buffer has sent and dropped.
func (c *Client) BufferStats() BufferStats {
	if c.buffer == nil {
		c.recordedMu.Lock()
		defer c.recordedMu.Unlock()
		return BufferStats{
			Queued:            len(c.recorded),
			DroppedOverflow:   c.recordedDropped,
			DroppedAfterClose: c.recordedClosed,
		}
	}
	return c.buffer.snapshotStats()
}

// Recorded returns the monitoring logs captured in test or offline mode, in the
// wire shape they would have been sent in.
func (c *Client) Recorded() []map[string]interface{} {
	c.recordedMu.Lock()
	defer c.recordedMu.Unlock()
	out := make([]map[string]interface{}, len(c.recorded))
	copy(out, c.recorded)
	return out
}

// ClearRecorded empties the captured monitoring logs and resets their overflow
// counter, so one test never reads another's state.
func (c *Client) ClearRecorded() {
	c.recordedMu.Lock()
	c.recorded = nil
	c.recordedDropped = 0
	c.recordedClosed = 0
	c.recordedMu.Unlock()
}

// Track times a provider call and logs it.
//
// Give it the use-case evidence, what you know about the call, and a function that
// actually talks to the provider. Whatever the function returns comes back
// unchanged — including its error, which propagates after the failure is
// logged. A panic is logged as an app error and re-panics.
func (c *Client) trackResolved(
	ctx context.Context,
	res *useCaseResolution,
	meta CallMeta,
	fn func(context.Context) (*Result, error),
) (*Result, error) {
	startedAt := c.cfg.now()
	start := time.Now()

	logRecord := func(outcome *Result, callErr *CallError) {
		rec := buildRecord(res, meta, outcome, callErr, startedAt, int(time.Since(start)/time.Millisecond))
		if err := c.Log(rec); err != nil {
			c.warnOnce("log-invalid", "could not log a generation: %v", err)
		}
	}

	logged := false
	defer func() {
		if r := recover(); r != nil {
			if !logged {
				logRecord(nil, &CallError{Kind: ErrorKindApp, Message: panicMessage(r)})
			}
			panic(r)
		}
	}()

	outcome, err := fn(ctx)
	logged = true
	if err != nil {
		logRecord(outcome, classifyError(err))
		return outcome, err
	}
	logRecord(outcome, nil)
	return outcome, nil
}

func buildRecord(res *useCaseResolution, meta CallMeta, outcome *Result, callErr *CallError, startedAt time.Time, latencyMS int) LogRecord {
	rec := LogRecord{
		ID:                meta.ID,
		useCaseResolution: res,
		StartedAt:         startedAt,
		LatencyMS:         latencyMS,
		latencyMeasured:   true,
		TraceID:           meta.TraceID,
		Sequence:          meta.Sequence,
		EndUserRef:        meta.EndUserRef,
		Environment:       meta.Environment,
		Status:            StatusOK,
	}
	if meta.Context != nil {
		rec.Context = meta.Context
	}
	if meta.Metadata != nil {
		rec.Metadata = meta.Metadata
	}
	if res != nil {
		rec.Params = mergeParams(res.Params, meta.Params)
	} else if meta.Params != nil {
		rec.Params = meta.Params
	}
	if meta.Variables != nil || meta.Messages != nil || meta.Text != "" {
		rec.Input = &Input{Variables: meta.Variables, Messages: meta.Messages, Text: meta.Text}
	}
	if outcome != nil {
		if outcome.Content != "" || outcome.ToolCalls != nil {
			rec.Output = &Output{Content: outcome.Content, ToolCalls: outcome.ToolCalls}
		}
		rec.FinishReason = outcome.FinishReason
		if outcome.StopKind != "" {
			rec.StopKind = NormalizeStopKind(string(outcome.StopKind))
		} else if outcome.FinishReason != "" {
			rec.StopKind = NormalizeStopKind(outcome.FinishReason)
		}
		rec.Usage = outcome.Usage
		rec.ModelUsed = outcome.ModelUsed
		rec.UpstreamProvider = outcome.UpstreamProvider
		if outcome.IsByok != nil {
			if rec.Metadata == nil {
				rec.Metadata = map[string]interface{}{}
			} else {
				rec.Metadata = deepCopyMap(rec.Metadata)
			}
			rec.Metadata["is_byok"] = *outcome.IsByok
		}
	}
	if callErr != nil {
		rec.Status = StatusError
		rec.Error = callErr
		if rec.Usage == nil {
			rec.Usage = &Usage{CostSource: CostSourceUnknown}
		}
	}
	return rec
}

func panicMessage(r interface{}) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	return valueToString(r)
}
