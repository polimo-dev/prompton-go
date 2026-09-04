package prompton

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
// POST /resolve

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
  "use_case": "greeting", "kind": "chat",
  "deployment": {"id": "0198f2a1-0000-7000-8000-00000000d001", "revision": 3},
  "prompt": "default", "prompts": ["default", "ko"],
  "model_id": "0198f2a1-0000-7000-8000-00000000e001",
  "model": "openai/gpt-4o-mini", "provider": "openrouter",
  "effective_params": {"temperature": 0.2},
  "effective_provider_options": {"only": ["OpenAI"]},
  "prompt_version": {"id": "0198f2a1-0000-7000-8000-00000000a001", "number": 2},
  "messages": [{"role": "system", "content": "You greet people."}, {"role": "user", "content": "Say hello to {{ name }}."}],
  "warnings": [], "etag": "sha256-abc"
}`

func TestResolveRemoteCachesAndRendersLocally(t *testing.T) {
	server := newResolveServer(t, resolveBody)
	c := newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: time.Hour,
	})

	for i := 0; i < 5; i++ {
		res, err := c.ResolveRemote(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
		if err != nil {
			t.Fatalf("ResolveRemote: %v", err)
		}
		if res.Messages[1].Content != "Say hello to Ada." {
			t.Fatalf("rendered %q", res.Messages[1].Content)
		}
		if res.Model != "openai/gpt-4o-mini" || res.DeploymentRevision != 3 {
			t.Fatalf("unexpected resolution: %+v", res)
		}
	}
	if got := atomic.LoadInt32(&server.calls); got != 1 {
		t.Fatalf("POST /resolve called %d times within the TTL, want 1", got)
	}
}

func TestResolveRemoteServesTheCachedAnswerOn429(t *testing.T) {
	server := newResolveServer(t, resolveBody)
	clock := newFakeClock()
	c := newTestClient(t, Config{
		Host: server.URL, APIKey: "ptn_sdkfixture_test", Environment: "production",
		Mode: ModeTest, CacheTTL: time.Second, now: clock.Now,
	})
	if _, err := c.ResolveRemote(testContext(t), "greeting"); err != nil {
		t.Fatalf("first ResolveRemote: %v", err)
	}
	atomic.StoreInt32(&server.status, 429)
	clock.Advance(2 * time.Second)

	res, err := c.ResolveRemote(testContext(t), "greeting", WithVariables(map[string]interface{}{"name": "Ada"}))
	if err != nil {
		t.Fatalf("a rate-limited resolve must serve the cached answer: %v", err)
	}
	if res.Messages[1].Content != "Say hello to Ada." {
		t.Fatalf("rendered %q from the cached template", res.Messages[1].Content)
	}
}

func TestResolveRemoteMapsA404ToTheSameErrorAsLocalResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no prompt","details":{"reason":"unknown_prompt","prompt":"fr","available_prompts":["default","ko"]}}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, Config{Host: srv.URL, APIKey: "ptn_sdkfixture_test", Mode: ModeTest})

	_, err := c.ResolveRemote(testContext(t), "greeting", WithPrompt("fr"))
	if !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("error %v, want ErrUnknownPrompt", err)
	}
	var re *ResolveError
	if !errors.As(err, &re) || re.Prompt != "fr" || len(re.AvailablePrompts) != 2 {
		t.Fatalf("the 404 details were lost: %+v", err)
	}
}

// ---------------------------------------------------------------------------
// resolution errors

func TestResolutionErrorsAreDistinguishable(t *testing.T) {
	snap, err := ParseSnapshot([]byte(testSnapshotJSON))
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if _, err := Resolve(snap, "nope"); !errors.Is(err, ErrUnknownUseCase) {
		t.Fatalf("want ErrUnknownUseCase, got %v", err)
	}
	if _, err := Resolve(snap, "greeting", WithPrompt("fr")); !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("want ErrUnknownPrompt, got %v", err)
	}
	if _, err := Resolve(snap, "greeting", WithVariables(map[string]interface{}{})); err == nil {
		t.Fatal("a missing variable must be an error")
	} else {
		var mv *MissingVariableError
		if !errors.As(err, &mv) || mv.Variable != "name" {
			t.Fatalf("want a MissingVariableError for name, got %v", err)
		}
	}
}

func TestResolveNeverFallsBackToDefaultPrompt(t *testing.T) {
	snap, _ := ParseSnapshot([]byte(testSnapshotJSON))
	if _, err := Resolve(snap, "greeting", WithPrompt("ko")); err == nil {
		t.Fatal("an unpinned prompt name must fail rather than quietly serve the default")
	}
}

func TestRenderIsPureAndRepeatable(t *testing.T) {
	snap, _ := ParseSnapshot([]byte(testSnapshotJSON))
	res, err := Resolve(snap, "greeting")
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
		t.Fatalf("rendering mutated the resolution: %q then %q", first[1].Content, second[1].Content)
	}
	if res.Messages[1].Content != "Say hello to {{ name }}." {
		t.Fatalf("the original template was overwritten: %q", res.Messages[1].Content)
	}
}

// ---------------------------------------------------------------------------
// UUIDv7

func TestUUIDv7ShapeAndOrdering(t *testing.T) {
	id := NewGenerationID()
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
	if _, ok := GenerationIDTime(id); !ok {
		t.Fatalf("id %q carries no timestamp", id)
	}

	earlier := newUUIDv7At(time.UnixMilli(1_700_000_000_000))
	later := newUUIDv7At(time.UnixMilli(1_700_000_001_000))
	if !(earlier < later) {
		t.Fatalf("ids from different milliseconds must sort by time: %q then %q", earlier, later)
	}
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewGenerationID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if _, ok := GenerationIDTime("not-a-uuid"); ok {
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
	if err := c.SetSnapshot([]byte(hashPolicy)); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	res := mustResolve(t, c, "greeting")
	if err := c.Log(GenerationRecord{
		Resolution: res,
		Status:     StatusOK,
		StartedAt:  time.Now(),
		Input:      &Input{Text: "a secret prompt"},
		Output:     &Output{Content: "a secret answer"},
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
