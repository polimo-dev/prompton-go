package prompton

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
