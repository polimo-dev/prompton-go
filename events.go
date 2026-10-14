package prompton

import (
	"context"
	"fmt"
	"time"
)

// TraceEvent is one observed tool or completion event. Provider-native JSON
// fields are preserved.
type TraceEvent map[string]interface{}

// Trace event kinds.
const (
	EventKindToolAttempt = "tool_attempt"
	EventKindCompletion  = "completion"
)

// Trace event statuses.
const (
	EventStatusStarted    = "started"
	EventStatusOK         = "ok"
	EventStatusError      = "error"
	EventStatusDenied     = "denied"
	EventStatusCancelled  = "cancelled"
	EventStatusTimeout    = "timeout"
	EventStatusMissing    = "missing"
	EventStatusIncomplete = "incomplete"
)

// LogEvents submits observed tool/completion trace events synchronously. The SDK
// never calls customer tools; it only sends the events the app observed.
func (c *Client) LogEvents(ctx context.Context, events []TraceEvent) (*BatchResult, error) {
	if len(events) == 0 {
		return &BatchResult{}, nil
	}
	if len(events) > 500 {
		return nil, fmt.Errorf("prompton: LogEvents accepts at most 500 events")
	}
	if c.cfg.Mode != ModeLive {
		return nil, ErrNoAPIKey
	}
	out := make([]map[string]interface{}, 0, len(events))
	for i := range events {
		if err := fillTraceEvent(c.cfg.now(), i, events[i]); err != nil {
			return nil, err
		}
		if isClosedTransportTraceEvent(events[i]) {
			continue
		}
		out = append(out, map[string]interface{}(events[i]))
	}
	if len(out) == 0 {
		return &BatchResult{}, nil
	}
	return c.postEvents(ctx, c.cfg.Environment, out)
}

func fillTraceEvent(now time.Time, index int, event TraceEvent) error {
	if event == nil {
		return fmt.Errorf("prompton: trace event %d is nil", index)
	}
	if stringField(event, "event_id") == "" {
		event["event_id"] = NewLogID()
	}
	if stringField(event, "observed_at") == "" {
		event["observed_at"] = now.UTC().Format(time.RFC3339Nano)
	}
	if stringField(event, "trace_id") == "" {
		return fmt.Errorf("prompton: trace event %d needs trace_id", index)
	}
	kind := stringField(event, "event_kind")
	if kind != EventKindToolAttempt && kind != EventKindCompletion {
		return fmt.Errorf("prompton: trace event %d has unsupported event_kind", index)
	}
	status := stringField(event, "status")
	switch status {
	case EventStatusStarted, EventStatusOK, EventStatusError, EventStatusDenied, EventStatusCancelled, EventStatusTimeout, EventStatusMissing, EventStatusIncomplete:
	default:
		return fmt.Errorf("prompton: trace event %d has unsupported status", index)
	}
	if _, ok := event["sdk"]; !ok {
		event["sdk"] = map[string]interface{}{"name": SDKName, "version": Version}
	}
	return nil
}

func stringField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func isClosedTransportTraceEvent(event map[string]interface{}) bool {
	if event["event_kind"] != EventKindCompletion || event["status"] != EventStatusError {
		return false
	}
	message, _ := event["completion_output"].(string)
	return isClosedTransportMessage(message) ||
		message == "failed to call LLM: failed to send request: %Req.TransportError{reason: :closed}"
}
