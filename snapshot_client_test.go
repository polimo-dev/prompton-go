package prompton

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// snapshotServer is a stub PromptOn: it serves one snapshot with a real ETag
// and lets a test script the next few responses.
type snapshotServer struct {
	*httptest.Server

	mu           sync.Mutex
	body         string
	etag         string
	requests     int32
	conditional  int32
	statusQueue  []int
	retryAfter   string
	lastIfNone   string
	generations  [][]map[string]interface{}
	genRaw       [][]byte
	genEnvs      []string
	genStatus    []int
	genRetryHdr  string
	genResponses []string
}

func newSnapshotServer(t *testing.T, body string) *snapshotServer {
	t.Helper()
	s := &snapshotServer{body: body}
	sum := sha256.Sum256([]byte(body))
	s.etag = `"sha256-` + hex.EncodeToString(sum[:]) + `"`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/v1/generations", s.handleGenerations)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *snapshotServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&s.requests, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastIfNone = r.Header.Get("If-None-Match")
	if s.lastIfNone != "" {
		atomic.AddInt32(&s.conditional, 1)
	}
	if len(s.statusQueue) > 0 {
		status := s.statusQueue[0]
		s.statusQueue = s.statusQueue[1:]
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down","details":{}}}`))
		return
	}
	w.Header().Set("ETag", s.etag)
	w.Header().Set("Cache-Control", "max-age=30")
	if s.lastIfNone == s.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(s.body))
}

func (s *snapshotServer) handleGenerations(w http.ResponseWriter, r *http.Request) {
	var envelope struct {
		Generations []map[string]interface{} `json:"generations"`
	}
	buf := make([]byte, 0)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	_ = decodeJSON(buf, &envelope)

	s.mu.Lock()
	s.generations = append(s.generations, envelope.Generations)
	s.genRaw = append(s.genRaw, buf)
	s.genEnvs = append(s.genEnvs, r.URL.Query().Get("environment"))
	status := 202
	if len(s.genStatus) > 0 {
		status = s.genStatus[0]
		s.genStatus = s.genStatus[1:]
	}
	body := ""
	if len(s.genResponses) > 0 {
		body = s.genResponses[0]
		s.genResponses = s.genResponses[1:]
	}
	retry := s.genRetryHdr
	s.mu.Unlock()

	if retry != "" && status != 202 {
		w.Header().Set("Retry-After", retry)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == "" {
		if status == 202 {
			body = `{"accepted":` + itoaTest(len(envelope.Generations)) + `,"duplicates":0,"rejected":[]}`
		} else {
			body = `{"error":{"code":"unavailable","message":"nope","details":{}}}`
		}
	}
	_, _ = w.Write([]byte(body))
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (s *snapshotServer) snapshotRequests() int { return int(atomic.LoadInt32(&s.requests)) }

func (s *snapshotServer) conditionalRequests() int { return int(atomic.LoadInt32(&s.conditional)) }

func (s *snapshotServer) queueSnapshotStatuses(retryAfter string, statuses ...int) {
	s.mu.Lock()
	s.statusQueue = append(s.statusQueue, statuses...)
	s.retryAfter = retryAfter
	s.mu.Unlock()
}

// rawBatches is the exact bytes each /generations request carried, which is
// the only way to see what actually went on the wire.
func (s *snapshotServer) rawBatches() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.genRaw))
	copy(out, s.genRaw)
	return out
}

// tracesByEnvironment groups the trace ids the server received by the
// environment query parameter the batch carried.
func (s *snapshotServer) tracesByEnvironment() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for i, batch := range s.generations {
		env := ""
		if i < len(s.genEnvs) {
			env = s.genEnvs[i]
		}
		for _, rec := range batch {
			trace, _ := rec["trace_id"].(string)
			out[env] = append(out[env], trace)
		}
	}
	return out
}

func (s *snapshotServer) batches() [][]map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]map[string]interface{}, len(s.generations))
	copy(out, s.generations)
	return out
}

func (s *snapshotServer) scriptGenerations(retryHeader string, statuses []int, bodies []string) {
	s.mu.Lock()
	s.genStatus = append(s.genStatus, statuses...)
	s.genResponses = append(s.genResponses, bodies...)
	s.genRetryHdr = retryHeader
	s.mu.Unlock()
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quietLogger(t)
	}
	if cfg.DiskCachePath == "" {
		cfg.DisableDiskCache = true
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitForRemoteSnapshot(t *testing.T, c *Client) {
	t.Helper()
	waitFor(t, 3*time.Second, "the first snapshot fetch", func() bool {
		return c.SnapshotInfo().Source == SourceRemote
	})
}

// ---------------------------------------------------------------------------

func TestSnapshotServedFromMemoryWithinTTL(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:        server.URL,
		APIKey:      "ptn_sdkfixture_test",
		Environment: "production",
		CacheTTL:    time.Hour,
	})
	waitForRemoteSnapshot(t, c)

	for i := 0; i < 50; i++ {
		res := mustResolve(t, c, "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
		if res.Messages[1].Content != "Say hello to Ada." {
			t.Fatalf("unexpected rendering %q", res.Messages[1].Content)
		}
	}
	if got := server.snapshotRequests(); got != 1 {
		t.Fatalf("expected exactly 1 snapshot request within the TTL, got %d", got)
	}
}

func TestSnapshotRepollsWithIfNoneMatch(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:        server.URL,
		APIKey:      "ptn_sdkfixture_test",
		Environment: "production",
		CacheTTL:    20 * time.Millisecond,
	})
	waitForRemoteSnapshot(t, c)
	waitFor(t, 3*time.Second, "a conditional repoll", func() bool {
		return server.conditionalRequests() >= 2
	})
	// A 304 changes nothing: the document and its ETag stay put.
	info := c.SnapshotInfo()
	if info.ETag == "" || info.Source != SourceRemote || info.Stale {
		t.Fatalf("unexpected snapshot info after a 304: %+v", info)
	}
	mustResolve(t, c, "greeting")
}

func TestSnapshotRateLimitHonoursRetryAfter(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	clock := newFakeClock()
	c := newTestClient(t, Config{
		Host:        server.URL,
		APIKey:      "ptn_sdkfixture_test",
		Environment: "production",
		CacheTTL:    5 * time.Millisecond,
		Logger:      quietLogger(t),
		now:         clock.Now,
	})
	waitForRemoteSnapshot(t, c)
	server.queueSnapshotStatuses("60", 429, 429, 429)

	waitFor(t, 3*time.Second, "the rate-limited poll", func() bool {
		return c.SnapshotInfo().Stale
	})
	before := server.snapshotRequests()
	time.Sleep(80 * time.Millisecond)
	if after := server.snapshotRequests(); after > before+1 {
		t.Fatalf("kept polling through Retry-After: %d then %d requests", before, after)
	}
	// The caller never sees the rate limit.
	if _, err := c.Resolve(testContext(t), "greeting"); err != nil {
		t.Fatalf("resolve failed while rate limited: %v", err)
	}
}

func TestSnapshotServerErrorKeepsServingPreviousDocument(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:        server.URL,
		APIKey:      "ptn_sdkfixture_test",
		Environment: "production",
		CacheTTL:    5 * time.Millisecond,
	})
	waitForRemoteSnapshot(t, c)
	server.queueSnapshotStatuses("", 500, 500, 500, 503)

	waitFor(t, 3*time.Second, "the failing poll", func() bool {
		return c.SnapshotInfo().Stale
	})
	res := mustResolve(t, c, "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if res.Model != "openai/gpt-4o-mini" {
		t.Fatalf("stale document did not resolve: %+v", res)
	}
}

func TestSnapshotServerDownFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	diskPath := writeFile(t, dir, "snapshot.json", testSnapshotJSON)

	// A host that refuses connections stands in for PromptOn being unreachable.
	c := newTestClient(t, Config{
		Host:          "http://127.0.0.1:1",
		APIKey:        "ptn_sdkfixture_test",
		Environment:   "production",
		Project:       "sdkfixture",
		DiskCachePath: diskPath,
		CacheTTL:      time.Hour,
		Timeout:       100 * time.Millisecond,
	})
	res := mustResolve(t, c, "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if res.Source != SourceDisk {
		t.Fatalf("resolution_source %q, want disk", res.Source)
	}
}

func TestSnapshotBundleUsedWhenDiskIsEmpty(t *testing.T) {
	dir := t.TempDir()
	bundle := writeFile(t, dir, "bundle.json", testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:          "http://127.0.0.1:1",
		APIKey:        "ptn_sdkfixture_test",
		Environment:   "production",
		Project:       "sdkfixture",
		DiskCachePath: filepath.Join(dir, "missing.json"),
		BundlePath:    bundle,
		Timeout:       100 * time.Millisecond,
	})
	res := mustResolve(t, c, "greeting")
	if res.Source != SourceBundle {
		t.Fatalf("resolution_source %q, want bundle", res.Source)
	}
}

func TestSnapshotFromAnotherEnvironmentIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		document string
	}{
		{"another environment", stagingSnapshotJSON()},
		// A hand-assembled or legacy bundle that names no environment is
		// refused too: an unlabelled document would otherwise be accepted by
		// every process, whatever it reads.
		{"no environment named", unlabelledSnapshotJSON()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bundle := writeFile(t, dir, "bundle.json", tc.document)
			c := newTestClient(t, Config{
				Host:             "http://127.0.0.1:1",
				APIKey:           "ptn_sdkfixture_test",
				Environment:      "production",
				Project:          "sdkfixture",
				BundlePath:       bundle,
				DisableDiskCache: true,
				Timeout:          100 * time.Millisecond,
			})
			if _, err := c.Resolve(testContext(t), "greeting"); !errors.Is(err, ErrNotReady) {
				t.Fatalf("the document must not boot a production process, got %v", err)
			}
		})
	}
}

func TestSnapshotFromAnotherProjectIsRefused(t *testing.T) {
	dir := t.TempDir()
	bundle := writeFile(t, dir, "bundle.json", testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:             "http://127.0.0.1:1",
		APIKey:           "ptn_otherproject_test",
		Environment:      "production",
		Project:          "otherproject",
		BundlePath:       bundle,
		DisableDiskCache: true,
		Timeout:          100 * time.Millisecond,
	})
	if _, err := c.Resolve(testContext(t), "greeting"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a document from another project must not be used, got %v", err)
	}
}

func TestCorruptDiskCacheIsIgnored(t *testing.T) {
	dir := t.TempDir()
	diskPath := writeFile(t, dir, "snapshot.json", `{"schema_version":3,"use_ca`)
	bundle := writeFile(t, dir, "bundle.json", testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:          "http://127.0.0.1:1",
		APIKey:        "ptn_sdkfixture_test",
		Environment:   "production",
		Project:       "sdkfixture",
		DiskCachePath: diskPath,
		BundlePath:    bundle,
		Timeout:       100 * time.Millisecond,
	})
	res := mustResolve(t, c, "greeting")
	if res.Source != SourceBundle {
		t.Fatalf("a half-written disk cache must fall through to the bundle, got %q", res.Source)
	}
}

func TestSnapshotIsMirroredToDiskAtomically(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "nested", "snapshot.json")
	c := newTestClient(t, Config{
		Host:          server.URL,
		APIKey:        "ptn_sdkfixture_test",
		Environment:   "production",
		DiskCachePath: diskPath,
		CacheTTL:      time.Hour,
	})
	waitForRemoteSnapshot(t, c)
	// The body is written before its sidecar, so a reader that arrives between
	// the two renames sees a snapshot with no ETag and simply refetches once.
	waitFor(t, 2*time.Second, "the disk mirror and its sidecar", func() bool {
		_, err := os.Stat(sidecarPath(diskPath))
		return err == nil
	})

	body, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read disk cache: %v", err)
	}
	if string(body) != testSnapshotJSON {
		t.Fatal("the disk cache must hold the server's bytes unchanged: the ETag is a hash of them")
	}
	meta, err := os.ReadFile(sidecarPath(diskPath))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var sc sidecar
	if err := decodeJSON(meta, &sc); err != nil {
		t.Fatalf("decode sidecar: %v", err)
	}
	if sc.ETag == "" || sc.Environment != "production" || sc.Project != "sdkfixture" {
		t.Fatalf("sidecar is missing the ETag or the guard fields: %+v", sc)
	}
	// No temporary files left behind by the tmp-then-rename write.
	entries, _ := os.ReadDir(filepath.Dir(diskPath))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestNoAPIKeyMakesNoRemoteCalls(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	dir := t.TempDir()
	bundle := writeFile(t, dir, "bundle.json", testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:             server.URL,
		Environment:      "production",
		Project:          "sdkfixture",
		BundlePath:       bundle,
		DisableDiskCache: true,
	})
	res := mustResolve(t, c, "greeting")
	if res.Source != SourceBundle {
		t.Fatalf("without an API key the SDK must work from the bundle, got %q", res.Source)
	}
	time.Sleep(50 * time.Millisecond)
	if server.snapshotRequests() != 0 {
		t.Fatalf("made %d remote call(s) without an API key", server.snapshotRequests())
	}
	if err := c.Log(sampleRecord(1)); err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(c.Recorded()) != 1 {
		t.Fatalf("expected the record to be captured in memory, got %d", len(c.Recorded()))
	}
}

func TestOfflineModeNeverCallsTheServer(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	dir := t.TempDir()
	diskPath := writeFile(t, dir, "snapshot.json", testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:          server.URL,
		APIKey:        "ptn_sdkfixture_test",
		Environment:   "production",
		Mode:          ModeOffline,
		DiskCachePath: diskPath,
	})
	res := mustResolve(t, c, "greeting")
	if res.Source != SourceDisk {
		t.Fatalf("offline mode must read the disk cache, got %q", res.Source)
	}
	if err := c.Refresh(testContext(t)); err != nil {
		t.Fatalf("offline refresh: %v", err)
	}
	if err := c.Log(sampleRecord(1)); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if server.snapshotRequests() != 0 || len(server.batches()) != 0 {
		t.Fatal("offline mode made a remote call")
	}
	if len(c.Recorded()) != 1 {
		t.Fatalf("offline mode should keep records in memory, got %d", len(c.Recorded()))
	}
}

func TestTestModeCapturesLogsAndUsesInjectedSnapshot(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if _, err := c.Resolve(testContext(t), "greeting"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("an empty test client should report not ready, got %v", err)
	}
	if err := c.SetSnapshot([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if res.Source != SourceManual {
		t.Fatalf("an injected document is resolution_source manual, got %q", res.Source)
	}
	if err := c.Log(GenerationRecord{
		Resolution: res,
		Status:     StatusOK,
		StartedAt:  time.Now(),
		Input:      &Input{Variables: map[string]interface{}{"name": "Ada"}},
		Output:     &Output{Content: "Hello, Ada!"},
	}); err != nil {
		t.Fatalf("log: %v", err)
	}
	logs := c.Recorded()
	if len(logs) != 1 {
		t.Fatalf("expected 1 captured log, got %d", len(logs))
	}
	if logs[0]["use_case"] != "greeting" || logs[0]["model"] != "openai/gpt-4o-mini" {
		t.Fatalf("the resolution did not fill the record: %v", logs[0])
	}
	if logs[0]["resolution_source"] != "manual" {
		t.Fatalf("resolution_source %v, want manual", logs[0]["resolution_source"])
	}
	c.ClearRecorded()
	if len(c.Recorded()) != 0 {
		t.Fatal("ClearRecorded left records behind")
	}
}

func TestExportSnapshotProducesALoadableBundle(t *testing.T) {
	server := newSnapshotServer(t, testSnapshotJSON)
	c := newTestClient(t, Config{
		Host:        server.URL,
		APIKey:      "ptn_sdkfixture_test",
		Environment: "production",
		CacheTTL:    time.Hour,
	})
	waitForRemoteSnapshot(t, c)

	out := filepath.Join(t.TempDir(), "prompton", "snapshot.production.json")
	if err := c.ExportSnapshot(out); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	entry, err := readSnapshotFile(out, "production", "sdkfixture")
	if err != nil {
		t.Fatalf("the exported bundle does not load back: %v", err)
	}
	if entry.snapshot.Deployments["greeting"] == nil {
		t.Fatal("the exported bundle lost its deployments")
	}
}

func TestUnsupportedSchemaVersionIsRefused(t *testing.T) {
	_, err := ParseSnapshot([]byte(`{"schema_version":2,"use_cases":{}}`))
	var schemaErr *UnsupportedSchemaError
	if !errors.As(err, &schemaErr) || schemaErr.Version != 2 {
		t.Fatalf("a v2 snapshot must be refused, got %v", err)
	}
}
