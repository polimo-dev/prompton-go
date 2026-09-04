package prompton

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConfigPrecedenceExplicitOverEnvOverDefault(t *testing.T) {
	t.Setenv("PTN_HOST", "https://env.example")
	t.Setenv("PTN_API_KEY", "ptn_envproject_key")
	t.Setenv("PTN_ENVIRONMENT", "staging")

	cfg := Config{Host: "https://explicit.example"}
	resolved, err := cfg.withDefaults()
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if resolved.Host != "https://explicit.example" {
		t.Fatalf("Host %q: an explicit option must beat the environment", resolved.Host)
	}
	if resolved.APIKey != "ptn_envproject_key" {
		t.Fatalf("APIKey %q: the environment must fill an empty option", resolved.APIKey)
	}
	if resolved.Environment != "staging" {
		t.Fatalf("Environment %q, want staging from the environment", resolved.Environment)
	}
	if resolved.Project != "envproject" {
		t.Fatalf("Project %q: the project should be read out of the API key", resolved.Project)
	}
	if resolved.baseURL() != "https://explicit.example/api/v1" {
		t.Fatalf("baseURL %q: the SDK appends /api/v1", resolved.baseURL())
	}

	os.Unsetenv("PTN_HOST")
	os.Unsetenv("PTN_API_KEY")
	os.Unsetenv("PTN_ENVIRONMENT")
	bare, err := (&Config{}).withDefaults()
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if bare.Host != DefaultHost || bare.Environment != DefaultEnvironment {
		t.Fatalf("defaults are wrong: %q %q", bare.Host, bare.Environment)
	}
	if bare.CacheTTL != 10*time.Second {
		t.Fatalf("CacheTTL %s, want the documented 10s default", bare.CacheTTL)
	}
	if bare.UserAgent != "prompton-go/"+Version {
		t.Fatalf("UserAgent %q", bare.UserAgent)
	}
}

func TestConfigRejectsAnUnknownMode(t *testing.T) {
	if _, err := New(Config{Mode: "sideways"}); err == nil {
		t.Fatal("an unknown mode must be an error")
	}
}

func TestBaseURLIsNotDoubled(t *testing.T) {
	cfg, _ := (&Config{Host: "https://app.example/api/v1/"}).withDefaults()
	if got := cfg.baseURL(); got != "https://app.example/api/v1" {
		t.Fatalf("baseURL %q", got)
	}
}

func TestUserAgentAndAuthorizationAreSent(t *testing.T) {
	var gotUA, gotAuth, gotEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotEnv = r.URL.Query().Get("environment")
		w.Header().Set("ETag", `"sha256-x"`)
		_, _ = w.Write([]byte(testSnapshotJSON))
	}))
	defer srv.Close()

	c := newTestClient(t, Config{Host: srv.URL, APIKey: "ptn_sdkfixture_secret", Environment: "production", CacheTTL: time.Hour})
	waitForRemoteSnapshot(t, c)
	if gotUA != "prompton-go/"+Version {
		t.Fatalf("User-Agent %q", gotUA)
	}
	if gotAuth != "Bearer ptn_sdkfixture_secret" {
		t.Fatalf("Authorization %q", gotAuth)
	}
	if gotEnv != "production" {
		t.Fatalf("environment query %q", gotEnv)
	}
}

// ---------------------------------------------------------------------------
// prompt endpoint

type resolveServer struct {
	*httptest.Server
	calls  int32
	status int32
	body   string
}

func newResolveServer(t *testing.T, body string) *resolveServer {
	t.Helper()
	s := &resolveServer{body: body}
	atomic.StoreInt32(&s.status, 200)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		status := int(atomic.LoadInt32(&s.status))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 200 {
			_, _ = w.Write([]byte(s.body))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down","details":{}}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

const resolveBody = `{
  "key": "greeting", "kind": "chat",
  "deployment": {"id": "0198f2a1-0000-7000-8000-00000000d001", "revision": 3},
  "prompt": "default", "prompt_names": ["default", "ko"],
  "model_id": "0198f2a1-0000-7000-8000-00000000e001",
  "model": "openai/gpt-4o-mini", "provider": "openrouter",
  "params": {"temperature": 0.2},
  "provider_options": {"only": ["OpenAI"]},
  "prompt_version": {"id": "0198f2a1-0000-7000-8000-00000000a001", "number": 2},
  "messages": [{"role": "system", "content": "You greet people."}, {"role": "user", "content": "Say hello to {{ name }}."}],
  "warnings": [], "etag": "sha256-abc"
}`

func TestRemoteUseCaseCachesAndRendersLocally(t *testing.T) {
	server := newResolveServer(t, resolveBody)
	c := newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: time.Hour,
	})

	for i := 0; i < 5; i++ {
		res, err := c.RemoteUseCase(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
		if err != nil {
			t.Fatalf("RemoteUseCase: %v", err)
		}
		messages, err := res.Messages(testContext(t), nil)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if messages[1].Content != "Say hello to Ada." {
			t.Fatalf("rendered %q", messages[1].Content)
		}
		if res.Model != "openai/gpt-4o-mini" || res.DeploymentRevision != 3 {
			t.Fatalf("unexpected use case: %+v", res)
		}
	}
	if got := atomic.LoadInt32(&server.calls); got != 1 {
		t.Fatalf("prompt endpoint called %d times within the TTL, want 1", got)
	}
}

func TestRemoteUseCaseDecodesSourceWithRemoteFallback(t *testing.T) {
	withSource := strings.Replace(resolveBody, `"etag": "sha256-abc"`, `"source": "disk", "etag": "sha256-abc"`, 1)
	server := newResolveServer(t, withSource)
	c := newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: 0,
	})
	res, err := c.RemoteUseCase(testContext(t), "greeting")
	if err != nil {
		t.Fatalf("RemoteUseCase: %v", err)
	}
	if res.Source != SourceDisk {
		t.Fatalf("source %q, want disk", res.Source)
	}

	server = newResolveServer(t, resolveBody)
	c = newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: 0,
	})
	res, err = c.RemoteUseCase(testContext(t), "greeting")
	if err != nil {
		t.Fatalf("RemoteUseCase: %v", err)
	}
	if res.Source != SourceRemote {
		t.Fatalf("source %q, want remote", res.Source)
	}
}

func TestRemoteUseCaseServesTheCachedAnswerOn429(t *testing.T) {
	server := newResolveServer(t, resolveBody)
	clock := newFakeClock()
	c := newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: time.Second, now: clock.Now,
	})
	if _, err := c.RemoteUseCase(testContext(t), "greeting"); err != nil {
		t.Fatalf("first RemoteUseCase: %v", err)
	}
	atomic.StoreInt32(&server.status, 429)
	clock.Advance(2 * time.Second)

	res, err := c.RemoteUseCase(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if err != nil {
		t.Fatalf("a rate-limited resolve must serve the cached answer: %v", err)
	}
	messages, err := res.Messages(testContext(t), nil)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if messages[1].Content != "Say hello to Ada." {
		t.Fatalf("rendered %q from the cached template", messages[1].Content)
	}
}

func TestRemoteUseCaseMapsA404ToTheSameErrorAsLocalUseCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no prompt","details":{"reason":"unknown_prompt","prompt":"fr","prompt_names":["default","ko"]}}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Host: srv.URL, APIKey: "ptn_sdkfixture_test", Mode: ModeTest})

	_, err := c.RemoteUseCase(testContext(t), "greeting", WithPrompt("fr"))
	if !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("error %v, want ErrUnknownPrompt", err)
	}
	var re *UseCaseError
	if !errors.As(err, &re) || re.Prompt != "fr" || len(re.PromptNames) != 2 {
		t.Fatalf("the 404 details were lost: %+v", err)
	}
}

// ---------------------------------------------------------------------------
// resolution errors

func TestUseCaseErrorsAreDistinguishable(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(testSnapshotJSON))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}
	if _, err := resolveSnapshot(snap, "nope"); !errors.Is(err, ErrUnknownUseCase) {
		t.Fatalf("want ErrUnknownUseCase, got %v", err)
	}
	if _, err := resolveSnapshot(snap, "greeting", WithPrompt("fr")); !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("want ErrUnknownPrompt, got %v", err)
	}
	if _, err := resolveSnapshot(snap, "greeting", WithVariables(map[string]interface{}{})); err == nil {
		t.Fatal("a missing variable must be an error")
	} else {
		var mv *MissingVariableError
		if !errors.As(err, &mv) || mv.Variable != "name" {
			t.Fatalf("want a MissingVariableError for name, got %v", err)
		}
	}
}

func TestResolveNeverFallsBackToDefaultPrompt(t *testing.T) {
	snap, _ := ParseUseCaseDocument([]byte(testSnapshotJSON))
	if _, err := resolveSnapshot(snap, "greeting", WithPrompt("fr")); err == nil {
		t.Fatal("an unpinned prompt name must fail rather than quietly serve the default")
	}
}

func TestRenderIsPureAndRepeatable(t *testing.T) {
	snap, _ := ParseUseCaseDocument([]byte(testSnapshotJSON))
	res, err := resolveSnapshot(snap, "greeting")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first, err := res.RenderMessages(map[string]interface{}{"name": "Ada"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := res.RenderMessages(map[string]interface{}{"name": "Bo"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if first[1].Content != "Say hello to Ada." || second[1].Content != "Say hello to Bo." {
		t.Fatalf("rendering mutated the use case: %q then %q", first[1].Content, second[1].Content)
	}
	if res.Messages[1].Content != "Say hello to {{ name }}." {
		t.Fatalf("the original template was overwritten: %q", res.Messages[1].Content)
	}
}

// ---------------------------------------------------------------------------
// UUIDv7

func TestUUIDv7ShapeAndOrdering(t *testing.T) {
	id := NewLogID()
	if len(id) != 36 {
		t.Fatalf("id %q is not 36 characters", id)
	}
	if id[14] != '7' {
		t.Fatalf("id %q is not version 7", id)
	}
	variant := id[19]
	if !strings.ContainsRune("89ab", rune(variant)) {
		t.Fatalf("id %q has variant nibble %q, want 8-b", id, string(variant))
	}
	if _, ok := LogIDTime(id); !ok {
		t.Fatalf("id %q carries no timestamp", id)
	}

	earlier := newUUIDv7At(time.UnixMilli(1_700_000_000_000))
	later := newUUIDv7At(time.UnixMilli(1_700_000_001_000))
	if !(earlier < later) {
		t.Fatalf("ids from different milliseconds must sort by time: %q then %q", earlier, later)
	}
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewLogID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if _, ok := LogIDTime("not-a-uuid"); ok {
		t.Fatal("a non-UUID must not report a timestamp")
	}
}

// ---------------------------------------------------------------------------
// template surface

func TestTemplateHelpersAreExported(t *testing.T) {
	out, err := RenderTemplate("Hello {{ name }}", map[string]interface{}{"name": "Ada"}, "liquid")
	if err != nil || out != "Hello Ada" {
		t.Fatalf("RenderTemplate = %q, %v", out, err)
	}
	raw, err := RenderTemplate("Hello {{ name }}", nil, "raw")
	if err != nil || raw != "Hello {{ name }}" {
		t.Fatalf("the raw engine must pass the source through: %q, %v", raw, err)
	}
	if reasons := LintTemplate("{{ s | upcase }}"); len(reasons) != 1 || reasons[0].Kind != "disallowed_filter" {
		t.Fatalf("lint reasons %+v", reasons)
	}
	if vars := TemplateVariables("{% for i in items %}{{ i }}{{ tone }}{% endfor %}"); len(vars) != 2 || vars[0] != "items" || vars[1] != "tone" {
		t.Fatalf("variables %v, want [items tone]", vars)
	}
}

// ---------------------------------------------------------------------------
// payload policy plumbed through the client

func TestPayloadPolicyFromTheSnapshotIsApplied(t *testing.T) {
	hashPolicy := strings.Replace(testSnapshotJSON, `"mode": "full"`, `"mode": "hash"`, 1)
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetUseCaseDocument([]byte(hashPolicy)); err != nil {
		t.Fatalf("SetUseCaseDocument: %v", err)
	}
	res := mustUseCase(t, c, "greeting")
	if err := c.Log(LogRecord{
		UseCaseEvidence: res,
		Status:          StatusOK,
		StartedAt:       time.Now(),
		Input:           &Input{Text: "a secret prompt"},
		Output:          &Output{Content: "a secret answer"},
	}); err != nil {
		t.Fatalf("log: %v", err)
	}
	rec := c.Recorded()[0]
	input, _ := rec["input"].(map[string]interface{})
	if input["hashed"] != true || input["sha256"] == nil {
		t.Fatalf("mode hash must replace the input with a digest, got %v", input)
	}
	if _, leaked := input["text"]; leaked {
		t.Fatal("the raw text was sent under a hash policy")
	}
}

func TestNormalizePolicyClampsAndDefaults(t *testing.T) {
	defaults := PayloadPolicy{Mode: PayloadFull, SampleRate: 1, MaxBytes: DefaultMaxBytes}
	got := normalizePolicy(&PayloadPolicy{Mode: "weird", SampleRate: 5, MaxBytes: -1}, defaults)
	if got.Mode != PayloadFull || got.SampleRate != 1 || got.MaxBytes != DefaultMaxBytes {
		t.Fatalf("normalized policy %+v", got)
	}
	none := normalizePolicy(nil, defaults)
	if none.Mode != PayloadFull || none.MaxBytes != DefaultMaxBytes {
		t.Fatalf("a nil policy must fall back to the defaults: %+v", none)
	}
}

func TestCanonicalJSONSortsKeysAndSkipsHTMLEscaping(t *testing.T) {
	got := string(canonicalJSON(map[string]interface{}{"b": 1, "a": "<&>"}))
	if got != `{"a":"<&>","b":1}` {
		t.Fatalf("canonical JSON %s", got)
	}
}

// A provider can hand back a byte that is not valid UTF-8 — a multi-byte
// character truncated by a token limit, a mangled tool argument. The server
// parses the request body as UTF-8 and refuses the whole thing if any of it is
// not, so the encoder substitutes U+FFFD and keeps the batch parseable: the one
// bad record can then be rejected on its own, which is what the contract says
// happens.
func TestCanonicalJSONSubstitutesInvalidUTF8(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"valid ascii", "hello", `"hello"`},
		{"valid multi-byte", "안녕 ☃", `"안녕 ☃"`},
		{"lone continuation byte", "x\xffy", "\"x�y\""},
		{"truncated multi-byte", "x\xed\xa0y", "\"x��y\""},
		{"invalid at the end", "x\xfe", "\"x�\""},
		{"only invalid bytes", "\xff\xfe", "\"��\""},
		{"control characters are escaped", "a\nb\x01", `"a\nb\u0001"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(canonicalJSON(tc.value))
			if got != tc.want {
				t.Fatalf("canonical JSON %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatal("the encoder emitted invalid UTF-8")
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("the encoder emitted unparseable JSON: %q", got)
			}
		})
	}
}

func TestResolveRefusesAnEnvironmentOtherThanTheClients(t *testing.T) {
	c := newTestClient(t, Config{Mode: ModeTest, Environment: "production"})
	if err := c.SetUseCaseDocument([]byte(testSnapshotJSON)); err != nil {
		t.Fatalf("SetUseCaseDocument: %v", err)
	}

	_, err := c.UseCase(testContext(t), "greeting", WithEnvironment("staging"))
	if !errors.Is(err, ErrEnvironmentMismatch) {
		t.Fatalf("a local resolve must refuse another environment, got %v", err)
	}
	var re *UseCaseError
	if !errors.As(err, &re) || re.Environment != "staging" || re.DocumentEnvironment != "production" {
		t.Fatalf("the mismatch must name both environments: %+v", err)
	}

	// The client's own environment is accepted, and so is saying nothing.
	if _, err := c.UseCase(testContext(t), "greeting", WithEnvironment("production")); err != nil {
		t.Fatalf("resolve for the client's own environment: %v", err)
	}
	if _, err := c.UseCase(testContext(t), "greeting"); err != nil {
		t.Fatalf("resolve without an environment: %v", err)
	}

	// The pure resolver guards on the document it was handed.
	snap, err := ParseUseCaseDocument([]byte(testSnapshotJSON))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}
	if _, err := resolveSnapshot(snap, "greeting", WithEnvironment("staging")); !errors.Is(err, ErrEnvironmentMismatch) {
		t.Fatalf("Resolve must refuse another environment, got %v", err)
	}
}

func TestRemoteUseCaseStillHonoursWithEnvironment(t *testing.T) {
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = decodeJSON(body, &req)
		env, _ := req["environment"].(string)
		asked.Store(env)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resolveBody))
	}))
	defer srv.Close()

	c := newTestClient(t, Config{
		Host: srv.URL, APIKey: "ptn_sdkfixture_test", Environment: "production", Mode: ModeTest,
	})
	if _, err := c.RemoteUseCase(testContext(t), "greeting", WithEnvironment("staging")); err != nil {
		t.Fatalf("RemoteUseCase: %v", err)
	}
	if got, _ := asked.Load().(string); got != "staging" {
		t.Fatalf("the server was asked for environment %q, want staging", got)
	}
}
