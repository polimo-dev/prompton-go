package prompton

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func newLoggingClient(t *testing.T, server *snapshotServer, tweak func(*Config)) (*Client, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := Config{
		Host:             server.URL,
		APIKey:           "ptn_sdkfixture_test",
		Environment:      "production",
		DisableDiskCache: true,
		CacheTTL:         time.Hour,
		LogFlushInterval: time.Hour,
		LogFlushSize:     10000,
		LogFlushBytes:    1 << 30,
		LogMaxBuffer:     10000,
		Logger:           quietLogger(t),
		now:              clock.Now,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, clock
}

func logN(t *testing.T, c *Client, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := c.Log(sampleRecord(i)); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
}

func TestLogRequiresTheFiveMandatoryFields(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest})
	if err := c.Log(GenerationRecord{Model: "m", Status: StatusOK, StartedAt: time.Now()}); err == nil {
		t.Fatal("a record with no use_case must be rejected")
	}
	if err := c.Log(GenerationRecord{UseCase: "u", Status: StatusOK, StartedAt: time.Now()}); err == nil {
		t.Fatal("a record with no model must be rejected")
	}
	if err := c.Log(GenerationRecord{UseCase: "u", Model: "m", Status: "weird", StartedAt: time.Now()}); err == nil {
		t.Fatal("a record with an unknown status must be rejected")
	}
	// An unset started_at defaults to now; one outside the window the server
	// accepts is refused here rather than coming back in `rejected`.
	if err := c.Log(GenerationRecord{UseCase: "u", Model: "m", Status: StatusOK}); err != nil {
		t.Fatalf("an unset started_at should default to now: %v", err)
	}
	if err := c.Log(GenerationRecord{UseCase: "u", Model: "m", Status: StatusOK, StartedAt: time.Now().Add(-8 * 24 * time.Hour)}); err == nil {
		t.Fatal("a started_at more than 7 days in the past must be rejected")
	}
	if err := c.Log(GenerationRecord{UseCase: "u", Model: "m", Status: StatusOK, StartedAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("a started_at more than 5 minutes in the future must be rejected")
	}
}

func TestLogFillsIDAndSDK(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest})
	if err := c.Log(GenerationRecord{UseCase: "u", Model: "m", Status: StatusOK, StartedAt: time.Now()}); err != nil {
		t.Fatalf("log: %v", err)
	}
	rec := c.Recorded()[0]
	id, _ := rec["id"].(string)
	if _, ok := GenerationIDTime(id); !ok {
		t.Fatalf("id %q is not a UUIDv7", id)
	}
	sdk, _ := rec["sdk"].(map[string]interface{})
	if sdk["name"] != SDKName || sdk["version"] != Version {
		t.Fatalf("sdk %v, want %s/%s", sdk, SDKName, Version)
	}
}

func TestBufferFlushesOnSizeTrigger(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.LogFlushSize = 3 })
	logN(t, c, 3)
	waitFor(t, 2*time.Second, "the size-triggered flush", func() bool {
		return len(server.batches()) == 1
	})
	if got := len(server.batches()[0]); got != 3 {
		t.Fatalf("batch of %d, want 3", got)
	}
}

func TestBufferFlushesOnByteTrigger(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.LogFlushBytes = 300 })
	logN(t, c, 3)
	waitFor(t, 2*time.Second, "the byte-triggered flush", func() bool {
		return len(server.batches()) >= 1
	})
}

func TestBufferFlushesOnTimeTrigger(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.LogFlushInterval = 20 * time.Millisecond })
	logN(t, c, 1)
	waitFor(t, 2*time.Second, "the time-triggered flush", func() bool {
		return len(server.batches()) == 1
	})
}

func TestBufferCapsBatchesAt200Records(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, nil)
	logN(t, c, 250)
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	batches := server.batches()
	if len(batches) != 2 || len(batches[0]) != 200 || len(batches[1]) != 50 {
		sizes := make([]int, len(batches))
		for i, b := range batches {
			sizes[i] = len(b)
		}
		t.Fatalf("batch sizes %v, want [200 50]", sizes)
	}
}

func TestBufferRetriesTheSameBatchAfter429(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("30", []int{429}, []string{""})
	c, clock := newLoggingClient(t, server, nil)
	logN(t, c, 3)

	if err := c.Flush(testContext(t)); err == nil {
		t.Fatal("a rate-limited flush must report that records are still queued")
	}
	if len(server.batches()) != 1 {
		t.Fatalf("expected one attempt before Retry-After elapsed, got %d", len(server.batches()))
	}
	if got := c.BufferStats().Queued; got != 3 {
		t.Fatalf("%d records queued after the 429, want 3", got)
	}
	// Nothing is sent until Retry-After has elapsed.
	if err := c.Flush(testContext(t)); err == nil {
		t.Fatal("flushed again before Retry-After elapsed")
	}
	if len(server.batches()) != 1 {
		t.Fatalf("contacted the server again during Retry-After: %d batches", len(server.batches()))
	}

	clock.Advance(31 * time.Second)
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush after Retry-After: %v", err)
	}
	batches := server.batches()
	if len(batches) != 2 {
		t.Fatalf("expected a resend, got %d batches", len(batches))
	}
	// The resend carries the same ids, so the server counts duplicates rather
	// than storing anything twice.
	for i := range batches[0] {
		if batches[0][i]["id"] != batches[1][i]["id"] {
			t.Fatalf("resend changed the ids: %v then %v", batches[0][i]["id"], batches[1][i]["id"])
		}
	}
}

func TestBufferRetriesAfter5xx(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("", []int{503}, []string{""})
	c, clock := newLoggingClient(t, server, nil)
	logN(t, c, 2)

	if err := c.Flush(testContext(t)); err == nil {
		t.Fatal("a 503 flush must report records still queued")
	}
	clock.Advance(2 * time.Second)
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush after backoff: %v", err)
	}
	if len(server.batches()) != 2 {
		t.Fatalf("expected one retry, got %d batches", len(server.batches()))
	}
}

func TestBufferSplitsA413BatchInHalf(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("", []int{413}, []string{`{"error":{"code":"payload_too_large","message":"too big","details":{}}}`})
	c, _ := newLoggingClient(t, server, nil)
	logN(t, c, 4)

	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	batches := server.batches()
	if len(batches) != 3 || len(batches[0]) != 4 || len(batches[1]) != 2 || len(batches[2]) != 2 {
		sizes := make([]int, len(batches))
		for i, b := range batches {
			sizes[i] = len(b)
		}
		t.Fatalf("batch sizes %v, want [4 2 2]", sizes)
	}
}

func TestBufferDropsOnOther4xx(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("", []int{400}, []string{`{"error":{"code":"invalid_request","message":"nope","details":{}}}`})
	c, _ := newLoggingClient(t, server, nil)
	logN(t, c, 3)

	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("a dropped batch still empties the queue: %v", err)
	}
	stats := c.BufferStats()
	if stats.DroppedClientError != 3 || stats.Queued != 0 {
		t.Fatalf("stats %+v, want 3 dropped and an empty queue", stats)
	}
	if len(server.batches()) != 1 {
		t.Fatalf("a 4xx must not be retried, got %d batches", len(server.batches()))
	}
}

func TestBufferNeverResendsAcceptedRecords(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("", []int{202},
		[]string{`{"accepted":2,"duplicates":0,"rejected":[{"index":0,"id":"x","code":"invalid_request","message":"started_at is more than 7 days in the past"}]}`})
	c, _ := newLoggingClient(t, server, nil)
	logN(t, c, 3)

	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(server.batches()) != 1 {
		t.Fatalf("partial acceptance must not trigger a resend, got %d batches", len(server.batches()))
	}
	if got := c.BufferStats().DroppedRejected; got != 1 {
		t.Fatalf("DroppedRejected %d, want 1", got)
	}
}

func TestBufferDropsOldestWhenFull(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.LogMaxBuffer = 5 })
	logN(t, c, 12)

	stats := c.BufferStats()
	if stats.Queued != 5 || stats.DroppedOverflow != 7 {
		t.Fatalf("stats %+v, want 5 queued and 7 dropped", stats)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	kept := server.batches()[0]
	// Drop-oldest keeps the newest records.
	if kept[0]["trace_id"] != "test:7" || kept[len(kept)-1]["trace_id"] != "test:11" {
		t.Fatalf("kept %v … %v, want test:7 … test:11", kept[0]["trace_id"], kept[len(kept)-1]["trace_id"])
	}
}

func TestBufferSendsOneBatchPerEnvironment(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, nil)
	rec := sampleRecord(1)
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	staging := sampleRecord(2)
	staging.Environment = "staging"
	if err := c.Log(staging); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(server.batches()) != 2 {
		t.Fatalf("expected one batch per environment, got %d", len(server.batches()))
	}
}

func TestCloseFlushesWhatIsQueued(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	clock := newFakeClock()
	c, err := New(Config{
		Host:             server.URL,
		APIKey:           "ptn_sdkfixture_test",
		Environment:      "production",
		DisableDiskCache: true,
		LogFlushInterval: time.Hour,
		LogFlushSize:     10000,
		Logger:           quietLogger(t),
		now:              clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logN(t, c, 4)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(server.batches()) != 1 || len(server.batches()[0]) != 4 {
		t.Fatalf("shutdown did not drain the queue: %v", server.batches())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

func TestRedactHookRunsLast(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) {
		cfg.Redact = func(rec map[string]interface{}) map[string]interface{} {
			delete(rec, "input")
			rec["metadata"] = map[string]interface{}{"redacted": true}
			return rec
		}
	})
	rec := sampleRecord(1)
	rec.Input = &Input{Text: "a secret prompt"}
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sent := server.batches()[0][0]
	if _, present := sent["input"]; present {
		t.Fatal("the redact hook did not remove the input")
	}
	meta, _ := sent["metadata"].(map[string]interface{})
	if meta["redacted"] != true {
		t.Fatalf("metadata %v, want the hook's value", meta)
	}
}

func TestHashEndUserReplacesTheReference(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.HashEndUser = true })
	rec := sampleRecord(1)
	rec.EndUserRef = "user-42"
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, _ := server.batches()[0][0]["end_user_ref"].(string)
	want := sha256Hex([]byte("user-42"))
	if got != want {
		t.Fatalf("end_user_ref %q, want the sha256 %q", got, want)
	}
}

func TestWithGenerationLogsASuccess(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetSnapshot([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))

	out, err := c.WithGeneration(testContext(t), res, CallMeta{
		Variables: map[string]interface{}{"name": "Ada"},
		Messages:  res.Messages,
		TraceID:   "job:1",
	}, func(ctx context.Context) (*Outcome, error) {
		return &Outcome{
			Content:      "Hello, Ada!",
			FinishReason: "stop",
			Usage:        &Usage{InputTokens: IntPtr(38), OutputTokens: IntPtr(9), CostUSD: Float64Ptr(0.000112), CostSource: CostSourceProvider},
		}, nil
	})
	if err != nil || out.Content != "Hello, Ada!" {
		t.Fatalf("WithGeneration returned %v, %v", out, err)
	}
	rec := c.Recorded()[0]
	if rec["status"] != "ok" || rec["stop_kind"] != "stop" || rec["use_case"] != "greeting" {
		t.Fatalf("unexpected record: %v", rec)
	}
	if rec["deployment_id"] != res.DeploymentID || rec["prompt"] != "default" {
		t.Fatalf("the resolution evidence is missing: %v", rec)
	}
	if _, ok := rec["latency_ms"]; !ok {
		t.Fatal("latency_ms was not measured")
	}
}

func TestWithGenerationLogsAFailureAndPropagatesIt(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetSnapshot([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting")

	sentinel := NewCallError(ErrorKindRateLimited, 429, "slow down")
	_, err := c.WithGeneration(testContext(t), res, CallMeta{}, func(ctx context.Context) (*Outcome, error) {
		return nil, sentinel
	})
	if !errors.Is(err, error(sentinel)) {
		t.Fatalf("the caller's error must propagate unchanged, got %v", err)
	}
	rec := c.Recorded()[0]
	if rec["status"] != "error" {
		t.Fatalf("status %v, want error", rec["status"])
	}
	e, _ := rec["error"].(map[string]interface{})
	if e["kind"] != ErrorKindRateLimited || e["status"] != 429 {
		t.Fatalf("error %v, want a rate_limited 429", e)
	}
}

func TestWithGenerationKeepsUsageOnAParseFailure(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetSnapshot([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting")

	_, err := c.WithGeneration(testContext(t), res, CallMeta{}, func(ctx context.Context) (*Outcome, error) {
		outcome := &Outcome{
			Content:      `{"greeting": "Hello`,
			FinishReason: "length",
			Usage:        &Usage{InputTokens: IntPtr(38), OutputTokens: IntPtr(512), CostSource: CostSourceProvider},
		}
		return outcome, NewCallError(ErrorKindParse, 0, "unexpected end of JSON input")
	})
	if err == nil {
		t.Fatal("expected the parse error to propagate")
	}
	rec := c.Recorded()[0]
	if rec["status"] != "error" || rec["stop_kind"] != "length" {
		t.Fatalf("unexpected record: %v", rec)
	}
	usage, _ := rec["usage"].(map[string]interface{})
	if usage["output_tokens"] != 512 {
		t.Fatalf("the spend was lost: %v", usage)
	}
	if _, ok := rec["output"]; !ok {
		t.Fatal("the partial output was lost")
	}
}

func TestWithGenerationLogsAPanicAndRepanics(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetSnapshot([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting")

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic must propagate")
			}
		}()
		_, _ = c.WithGeneration(testContext(t), res, CallMeta{}, func(ctx context.Context) (*Outcome, error) {
			panic("provider client exploded")
		})
	}()

	logs := c.Recorded()
	if len(logs) != 1 {
		t.Fatalf("expected the panic to be logged, got %d records", len(logs))
	}
	e, _ := logs[0]["error"].(map[string]interface{})
	if e["kind"] != ErrorKindApp || !strings.Contains(str(e["message"]), "exploded") {
		t.Fatalf("error %v, want an app error mentioning the panic", e)
	}
}

func TestOversizedRecordIsDroppedAtEnqueue(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, nil)
	rec := sampleRecord(1)
	rec.Metadata = map[string]interface{}{"blob": strings.Repeat("x", maxBatchBytes+10)}
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	stats := c.BufferStats()
	if stats.DroppedTooLarge != 1 || stats.Queued != 0 {
		t.Fatalf("stats %+v, want one record dropped as too large", stats)
	}
}

func TestBufferDropsABatchAfterTheAttemptBound(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("1", []int{503, 503, 503}, []string{"", "", ""})
	c, clock := newLoggingClient(t, server, func(cfg *Config) { cfg.LogMaxAttempts = 3 })
	logN(t, c, 2)

	for i := 0; i < 3; i++ {
		_ = c.Flush(testContext(t))
		clock.Advance(2 * time.Second)
	}
	stats := c.BufferStats()
	if stats.DroppedRetriesExhausted != 2 {
		t.Fatalf("stats %+v, want 2 records dropped after the attempt bound", stats)
	}
	if stats.Queued != 0 {
		t.Fatalf("the exhausted batch is still queued: %+v", stats)
	}
	if len(server.batches()) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(server.batches()))
	}
}

func TestSnapshotPayloadPolicyTruncatesBeforeSending(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, nil)
	waitForRemoteSnapshot(t, c)

	rec := sampleRecord(1)
	rec.Input = &Input{Text: strings.Repeat("a", 400_000)}
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	input, _ := server.batches()[0][0]["input"].(map[string]interface{})
	text, _ := input["text"].(string)
	if len(text) > DefaultMaxBytes {
		t.Fatalf("input.text is %d bytes, over the %d cap", len(text), DefaultMaxBytes)
	}
	if input["truncated"] != true {
		t.Fatalf("a truncated input must say so: %v", input)
	}
	if !strings.Contains(text, "[truncated ") {
		t.Fatalf("the truncation marker is missing from %q…", text[:40])
	}
}

// A record can carry a byte that is not valid UTF-8: a multi-byte character a
// token limit cut in half, a mangled tool argument. The server parses the whole
// request body as UTF-8, so one such byte on the wire would 400 the request and
// destroy every other record in the batch. The encoder substitutes U+FFFD
// instead, which keeps the batch parseable and leaves the server free to reject
// that one record if it wants to.
func TestBatchStaysParseableWhenARecordCarriesInvalidUTF8(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"lone continuation byte", "provider said: x\xffy done", "provider said: x�y done"},
		{"truncated multi-byte", "\xed\xa0 tail", "�� tail"},
		{"valid text is untouched", "안녕 ☃", "안녕 ☃"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newSnapshotServer(t, testSnapshotJSON)
			c, _ := newLoggingClient(t, server, nil)
			rec := sampleRecord(1)
			rec.Output = &Output{Content: tc.content}
			if err := c.Log(rec); err != nil {
				t.Fatalf("log: %v", err)
			}
			if err := c.Flush(testContext(t)); err != nil {
				t.Fatalf("flush: %v", err)
			}
			raw := server.rawBatches()
			if len(raw) != 1 {
				t.Fatalf("expected 1 request, got %d", len(raw))
			}
			if !utf8.Valid(raw[0]) {
				t.Fatal("the request body carried invalid UTF-8: the server would refuse the whole batch")
			}
			if !json.Valid(raw[0]) {
				t.Fatalf("the request body is not valid JSON: %q", raw[0])
			}
			output, _ := server.batches()[0][0]["output"].(map[string]interface{})
			if got, _ := output["content"].(string); got != tc.want {
				t.Fatalf("content %q, want %q", got, tc.want)
			}
		})
	}
}

// Without a buffer — test mode, offline mode, and a live client with no API key
// — records are captured in memory. That capture is bounded exactly like the
// send queue: a process that boots on a missing PTN_API_KEY has to ride the
// misconfiguration out, not grow until it is killed.
func TestCaptureIsBoundedWhenThereIsNoBuffer(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"live with no API key", Config{Environment: "production"}},
		{"offline", Config{Mode: ModeOffline, Environment: "production"}},
		{"test", Config{Mode: ModeTest, Environment: "production"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.LogMaxBuffer = 10
			c := newTestClient(t, cfg)
			logN(t, c, 100)

			captured := c.Recorded()
			if len(captured) != 10 {
				t.Fatalf("captured %d records, want at most LogMaxBuffer (10)", len(captured))
			}
			stats := c.BufferStats()
			if stats.Queued != 10 || stats.DroppedOverflow != 90 {
				t.Fatalf("stats %+v, want 10 queued and 90 dropped", stats)
			}
			// Drop-oldest: the newest ten survive.
			if captured[0]["trace_id"] != "test:90" || captured[9]["trace_id"] != "test:99" {
				t.Fatalf("kept %v … %v, want test:90 … test:99", captured[0]["trace_id"], captured[9]["trace_id"])
			}
			c.ClearRecorded()
			if stats := c.BufferStats(); stats.Queued != 0 || stats.DroppedOverflow != 0 {
				t.Fatalf("ClearRecorded left %+v behind", stats)
			}
		})
	}
}

// Log after Close must say so. Silently discarding the record would tell the
// caller it was accepted, and Flush already reports the same condition.
func TestLogAfterCloseIsReported(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		server := newSnapshotServer(t, testSnapshotJSON)
		c, _ := newLoggingClient(t, server, nil)
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := c.Log(sampleRecord(1)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Log after Close returned %v, want ErrClosed", err)
		}
		if got := c.BufferStats().DroppedAfterClose; got != 1 {
			t.Fatalf("DroppedAfterClose %d, want 1", got)
		}
		if err := c.Flush(testContext(t)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Flush after Close returned %v, want ErrClosed", err)
		}
	})

	// A client that captures instead of sending answers the same way.
	t.Run("captured", func(t *testing.T) {
		c := newTestClient(t, Config{Mode: ModeTest})
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := c.Log(sampleRecord(1)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Log after Close returned %v, want ErrClosed", err)
		}
		if len(c.Recorded()) != 0 {
			t.Fatal("a closed client must not capture the record either")
		}
		if got := c.BufferStats().DroppedAfterClose; got != 1 {
			t.Fatalf("DroppedAfterClose %d, want 1", got)
		}
	})
}

// Drop-oldest means oldest, not "whichever environment sorts first": records
// carry an enqueue sequence so a process logging to two environments still
// loses its genuinely oldest records when the queue is full.
func TestBufferDropsTheOldestRecordsAcrossEnvironments(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c, _ := newLoggingClient(t, server, func(cfg *Config) { cfg.LogMaxBuffer = 6 })

	// staging is logged first, so its records are the older ones — even though
	// "production" sorts before it.
	for i := 0; i < 5; i++ {
		rec := sampleRecord(i)
		rec.Environment = "staging"
		if err := c.Log(rec); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	for i := 5; i < 10; i++ {
		if err := c.Log(sampleRecord(i)); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	if stats := c.BufferStats(); stats.Queued != 6 || stats.DroppedOverflow != 4 {
		t.Fatalf("stats %+v, want 6 queued and 4 dropped", stats)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}

	traces := server.tracesByEnvironment()
	if got := strings.Join(traces["staging"], ","); got != "test:4" {
		t.Fatalf("staging kept %q, want only the newest staging record test:4", got)
	}
	if got := strings.Join(traces["production"], ","); got != "test:5,test:6,test:7,test:8,test:9" {
		t.Fatalf("production kept %q, want every production record", got)
	}
}

// Failures belong to an environment. A success on one must not report health
// while another is still backing off, and a requeued batch must give its bytes
// back to the lane it came from.
func TestFailuresAreCountedPerEnvironment(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	server.scriptGenerations("", []int{500}, []string{""})
	c, _ := newLoggingClient(t, server, nil)

	if err := c.Log(sampleRecord(1)); err != nil {
		t.Fatalf("log: %v", err)
	}
	staging := sampleRecord(2)
	staging.Environment = "staging"
	if err := c.Log(staging); err != nil {
		t.Fatalf("log: %v", err)
	}
	// production is sent first and fails; staging is sent next and succeeds.
	if err := c.Flush(testContext(t)); err == nil {
		t.Fatal("the flush must report the record the failing environment still holds")
	}

	stats := c.BufferStats()
	if stats.Failures != 1 {
		t.Fatalf("Failures %d: a success on one environment must not clear another's backoff", stats.Failures)
	}
	if stats.Queued != 1 || stats.Sent != 1 {
		t.Fatalf("stats %+v, want the failed record queued and the other one sent", stats)
	}
	assertLaneBytes(t, c.buffer)
}

// assertLaneBytes checks the byte counters against the records the lanes
// actually hold: the flush trigger reads them, so a retry that forgot to give
// its bytes back would under-count for the lifetime of the process.
func assertLaneBytes(t *testing.T, b *logBuffer) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, l := range b.lanes {
		pending := 0
		for _, r := range l.pending {
			pending += r.bytes
		}
		if l.pendingBytes != pending {
			t.Fatalf("lane %q pendingBytes %d, want %d", name, l.pendingBytes, pending)
		}
		ready := 0
		for _, batch := range l.ready {
			ready += batch.byteSize()
		}
		if l.readyBytes != ready {
			t.Fatalf("lane %q readyBytes %d, want %d", name, l.readyBytes, ready)
		}
	}
}
