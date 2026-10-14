package prompton

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// These tests run against a live PromptOn server. They are skipped unless
// PTN_API_KEY is set, so `go test ./...` stays hermetic.
//
//	PTN_HOST=http://localhost:4000 PTN_API_KEY=ptn_… go test -run TestLive ./...
func liveClient(t *testing.T, tweak func(*Config)) *Client {
	t.Helper()
	key := os.Getenv("PTN_API_KEY")
	if key == "" {
		t.Skip("set PTN_API_KEY (and PTN_HOST) to run the live integration tests")
	}
	cfg := Config{
		APIKey:           key,
		Host:             os.Getenv("PTN_HOST"),
		Environment:      "production",
		CacheTTL:         50 * time.Millisecond,
		DiskCachePath:    filepath.Join(t.TempDir(), "snapshot.json"),
		LogFlushInterval: time.Hour,
		LogFlushSize:     10000,
		Logger:           quietLogger(t),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitFor(t, 10*time.Second, "the first live snapshot", func() bool {
		return c.PromptDocumentInfo().Source == SourceRemote
	})
	return c
}

func TestLiveSnapshotAndConditionalRepoll(t *testing.T) {
	c := liveClient(t, nil)
	info := c.PromptDocumentInfo()
	if info.ETag == "" {
		t.Fatal("the server sent no ETag")
	}
	if info.Environment != "production" {
		t.Fatalf("environment %q", info.Environment)
	}
	snap := c.PromptDocument()
	for _, key := range []string{"greeting", "summarize", "embed"} {
		if _, ok := snap.Prompts[key]; !ok {
			t.Fatalf("the fixture project has no prompt %q", key)
		}
	}

	// A repoll sends If-None-Match and a 304 leaves the document and its ETag
	// exactly where they were.
	if err := c.Refresh(testContext(t)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	after := c.PromptDocumentInfo()
	if after.ETag != info.ETag || after.Stale {
		t.Fatalf("a 304 changed the cached document: %+v then %+v", info, after)
	}

	// The disk mirror is written and loads back under the same guards.
	waitFor(t, 5*time.Second, "the disk mirror", func() bool {
		_, err := os.Stat(sidecarPath(c.cfg.DiskCachePath))
		return err == nil
	})
	if _, err := readSnapshotFile(c.cfg.DiskCachePath, "production", c.Project()); err != nil {
		t.Fatalf("the mirrored snapshot does not load back: %v", err)
	}
}

func TestLiveLocalPromptMatchesServerPromptEndpoint(t *testing.T) {
	c := liveClient(t, nil)

	cases := []struct {
		name      string
		promptKey string
		template  string
		variables map[string]interface{}
	}{
		{"greeting/default", "greeting", "", map[string]interface{}{"name": "Ada", "language": "en"}},
		{"greeting/korean-branch", "greeting", "", map[string]interface{}{"name": "아다", "language": "ko"}},
		{"summarize", "summarize", "", map[string]interface{}{"items": []interface{}{"alpha", "beta", "gamma"}}},
		{"embed", "embed", "", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var opts []PromptOption
			if tc.template != "" {
				opts = append(opts, WithTemplate(tc.template))
			}
			if tc.variables != nil {
				opts = append(opts, WithVariables(tc.variables))
			}
			local, err := c.Prompt(testContext(t), tc.promptKey, opts...)
			if err != nil {
				t.Fatalf("local resolve: %v", err)
			}
			remote, err := c.postResolve(testContext(t), resolveRequest{
				Prompt:      tc.promptKey,
				Environment: "production",
				Template:    tc.template,
				Variables:   tc.variables,
			})
			if err != nil {
				t.Fatalf("render endpoint: %v", err)
			}
			server := resolutionFromResponse(tc.promptKey, remote)

			if local.Model != server.Model || local.ModelID != server.ModelID || local.Provider != server.Provider {
				t.Fatalf("model differs: local %+v, server %+v", local, server)
			}
			if local.DeploymentID != server.DeploymentID || local.DeploymentRevision != server.DeploymentRevision {
				t.Fatalf("deployment differs: %s/%s vs %s/%s",
					local.DeploymentID, local.DeploymentRevision, server.DeploymentID, server.DeploymentRevision)
			}
			if local.PromptVersionID != server.PromptVersionID || local.Template != server.Template {
				t.Fatalf("pin differs: %q/%q vs %q/%q", local.Template, local.PromptVersionID, server.Template, server.PromptVersionID)
			}
			if !reflect.DeepEqual(local.TemplateNames, server.TemplateNames) {
				t.Fatalf("template names differ: %v vs %v", local.TemplateNames, server.TemplateNames)
			}
			if string(canonicalJSON(local.Params)) != string(canonicalJSON(server.Params)) {
				t.Fatalf("params differ: %s vs %s", canonicalJSON(local.Params), canonicalJSON(server.Params))
			}
			if string(canonicalJSON(local.ProviderOptions)) != string(canonicalJSON(server.ProviderOptions)) {
				t.Fatalf("provider_options differ: %s vs %s",
					canonicalJSON(local.ProviderOptions), canonicalJSON(server.ProviderOptions))
			}
			if !reflect.DeepEqual(local.promptResolution.Messages, server.Messages) {
				t.Fatalf("rendered messages differ:\nlocal  %+v\nserver %+v", local.promptResolution.Messages, server.Messages)
			}
			if local.promptResolution.Text != server.Text {
				t.Fatalf("rendered text differs:\nlocal  %q\nserver %q", local.promptResolution.Text, server.Text)
			}
		})
	}
}

func TestLiveStagingEnvironmentResolvesSeparately(t *testing.T) {
	c := liveClient(t, func(cfg *Config) { cfg.Environment = "staging" })
	if c.PromptDocumentInfo().Environment != "staging" {
		t.Fatalf("environment %q, want staging", c.PromptDocumentInfo().Environment)
	}
	local, err := c.Prompt(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	remote, err := c.postResolve(testContext(t), resolveRequest{
		Prompt: "greeting", Environment: "staging", Variables: map[string]interface{}{"name": "Ada"},
	})
	if err != nil {
		t.Fatalf("render endpoint: %v", err)
	}
	if string(canonicalJSON(local.Params)) != string(canonicalJSON(remote.Params)) {
		t.Fatalf("staging params differ: %s vs %s", canonicalJSON(local.Params), canonicalJSON(remote.Params))
	}
}

func TestLiveErrorCases(t *testing.T) {
	c := liveClient(t, nil)

	if _, err := c.Prompt(testContext(t), "does_not_exist"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown prompt locally: %v", err)
	}
	if _, err := c.RenderPrompt(testContext(t), "does_not_exist"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown prompt remotely: %v", err)
	}

	_, err := c.Prompt(testContext(t), "greeting", WithTemplate("fr"))
	var re *PromptError
	var apiErr *APIError
	if !errors.As(err, &re) || re.Code != "unknown_template" {
		t.Fatalf("unknown prompt locally: %v", err)
	}
	_, err = c.RenderPrompt(testContext(t), "greeting", WithTemplate("fr"))
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Code != "invalid_request" || apiErr.Details["template"] != "fr" {
		t.Fatalf("non-default template remotely: %v", err)
	}

	// A missing variable is caught locally, and the server agrees.
	_, err = c.Prompt(testContext(t), "greeting", WithVariables(map[string]interface{}{}))
	var mv *MissingVariableError
	if !errors.As(err, &mv) || mv.Variable != "name" {
		t.Fatalf("missing variable locally: %v", err)
	}
	_, err = c.postResolve(testContext(t), resolveRequest{
		Prompt: "greeting", Environment: "production", Variables: map[string]interface{}{},
	})
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Details["missing_variable"] != "name" {
		t.Fatalf("missing variable remotely: %v", err)
	}

	// An unknown environment is a 404 naming it.
	_, err = c.fetchSnapshot(testContext(t), "nope", "")
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Details["environment"] != "nope" {
		t.Fatalf("unknown environment: %v", err)
	}

	// A wrong key is a 401 and nothing else.
	bad := newTestClient(t, Config{Host: os.Getenv("PTN_HOST"), APIKey: "ptn_sdkfixture_notarealkey", Mode: ModeTest})
	_, err = bad.fetchSnapshot(testContext(t), "production", "")
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("invalid key: %v", err)
	}
}

func TestLiveLogsBatchIsAcceptedThenDuplicated(t *testing.T) {
	c := liveClient(t, nil)
	res, err := c.Prompt(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The ids are issued here so the same batch can be replayed afterwards.
	okID := NewLogID()
	errID := NewLogID()
	startedAt := time.Now().UTC()

	// The provider call is stubbed: this SDK never calls a provider, and the
	// integration test must not either.
	_, err = res.Track(testContext(t), CallMeta{
		ID:        okID,
		Variables: map[string]interface{}{"name": "Ada"},
		Messages:  res.promptResolution.Messages,
		TraceID:   "go-sdk-integration",
		Context:   map[string]interface{}{"language": "en"},
		Metadata:  map[string]interface{}{"suite": "integration"},
	}, func(ctx context.Context) (*Result, error) {
		return &Result{
			Content:          "Hello, Ada! Lovely to see you.",
			FinishReason:     "stop",
			ModelUsed:        res.Model,
			UpstreamProvider: "OpenAI",
			Usage: &Usage{
				InputTokens:  IntPtr(38),
				OutputTokens: IntPtr(9),
				CostUSD:      Float64Ptr(0.000112),
				CostSource:   CostSourceProvider,
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("Track: %v", err)
	}

	if err := c.Log(LogRecord{
		ID:             errID,
		PromptEvidence: res,
		Status:         StatusError,
		StartedAt:      startedAt,
		LatencyMS:      120,
		Error:          NewCallError(ErrorKindRateLimited, 429, "rate limited by upstream provider"),
		Input:          &Input{Variables: map[string]interface{}{"name": "Ada"}, Messages: res.promptResolution.Messages},
	}); err != nil {
		t.Fatalf("log: %v", err)
	}

	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if stats := c.BufferStats(); stats.Sent != 2 || stats.DroppedRejected != 0 {
		t.Fatalf("buffer stats %+v, want 2 sent and nothing rejected", stats)
	}

	// The id is the idempotency key: resending it is counted, never stored twice.
	replay := []map[string]interface{}{
		{"id": okID, "prompt_key": "greeting", "model": res.Model, "status": StatusOK, "started_at": startedAt.Format(time.RFC3339Nano)},
		{"id": errID, "prompt_key": "greeting", "model": res.Model, "status": StatusOK, "started_at": startedAt.Format(time.RFC3339Nano)},
	}
	result, err := c.postLogs(testContext(t), "production", replay)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if result.Duplicates != 2 || result.Accepted != 0 || len(result.Rejected) != 0 {
		t.Fatalf("resend result %+v, want 2 duplicates", result)
	}
}

// A provider can return a byte that is not valid UTF-8. The server parses the
// request body as UTF-8 and refuses the whole thing if any of it is not, so
// this proves against the real server that one such record no longer destroys
// the batch it travelled in.
func TestLiveLogWithInvalidUTF8IsStillAccepted(t *testing.T) {
	c := liveClient(t, nil)
	res, err := c.Prompt(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rec := LogRecord{
		PromptEvidence: res,
		Status:         StatusOK,
		StartedAt:      time.Now().UTC(),
		LatencyMS:      17,
		TraceID:        "go-sdk-integration-utf8",
		Output:         &Output{Content: "provider said: x\xff\xfey done"},
	}
	if err := c.Log(rec); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := c.Flush(testContext(t)); err != nil {
		t.Fatalf("a record carrying invalid UTF-8 must not fail the batch: %v", err)
	}
	stats := c.BufferStats()
	if stats.Sent != 1 || stats.DroppedClientError != 0 || stats.DroppedRejected != 0 {
		t.Fatalf("buffer stats %+v, want the record accepted and nothing dropped", stats)
	}
}
