package prompton

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The conformance suite is the cross-language contract: the same JSON files
// every PromptOn SDK replays. When two SDKs disagree about how a prompt
// renders, which model a snapshot resolves to, or how a monitoring log is
// truncated, an app that talks to PromptOn from two languages gets two
// different answers. These tests are what prevent that.

func loadConformance(t *testing.T, name string, v interface{}) {
	t.Helper()
	path := filepath.Join("testdata", "conformance", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := decodeJSON(data, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// stop_kind.json

func TestConformanceStopKind(t *testing.T) {
	var suite struct {
		Cases []struct {
			FinishReason *string `json:"finish_reason"`
			StopKind     string  `json:"stop_kind"`
			Truncated    bool    `json:"truncated"`
			Source       string  `json:"source"`
		} `json:"cases"`
	}
	loadConformance(t, "stop_kind.json", &suite)
	if len(suite.Cases) == 0 {
		t.Fatal("no stop_kind cases loaded")
	}
	for _, c := range suite.Cases {
		reason := ""
		if c.FinishReason != nil {
			reason = *c.FinishReason
		}
		got := NormalizeStopKind(reason)
		if string(got) != c.StopKind {
			t.Errorf("finish_reason %q (%s): stop_kind %q, want %q", reason, c.Source, got, c.StopKind)
		}
		if got.Truncated() != c.Truncated {
			t.Errorf("finish_reason %q: truncated %v, want %v", reason, got.Truncated(), c.Truncated)
		}
	}
}

// ---------------------------------------------------------------------------
// use_case.json

func TestConformanceUseCase(t *testing.T) {
	var suite struct {
		Documents map[string]json.RawMessage `json:"documents"`
		Cases     []struct {
			Name        string                 `json:"name"`
			DocumentRef string                 `json:"document_ref"`
			UseCase     string                 `json:"use_case"`
			Prompt      string                 `json:"prompt"`
			Variables   map[string]interface{} `json:"variables"`
			Environment string                 `json:"environment"`
			Expect      map[string]interface{} `json:"expect"`
		} `json:"cases"`
	}
	loadConformance(t, "use_case.json", &suite)
	if len(suite.Cases) == 0 {
		t.Fatal("no use-case cases loaded")
	}

	documents := map[string]*UseCaseDocument{}
	for name, raw := range suite.Documents {
		snap, err := ParseUseCaseDocument(raw)
		if err != nil {
			t.Fatalf("parse document %s: %v", name, err)
		}
		documents[name] = snap
	}

	for _, c := range suite.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			snap := documents[c.DocumentRef]
			if snap == nil {
				t.Fatalf("unknown document_ref %q", c.DocumentRef)
			}
			var opts []UseCaseOption
			if c.Prompt != "" {
				opts = append(opts, WithPrompt(c.Prompt))
			}
			if c.Variables != nil {
				opts = append(opts, WithVariables(c.Variables))
			}
			res, err := resolveSnapshot(snap, c.UseCase, opts...)

			if wantErr, ok := c.Expect["error"].(string); ok {
				if err == nil {
					t.Fatalf("expected error %q, got a resolution", wantErr)
				}
				assertUseCaseError(t, err, wantErr, c.Expect)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			res.Source = SourceRemote
			got := useCaseSelectionToMap(res)
			gotJSON := string(canonicalJSON(got))
			wantJSON := string(canonicalJSON(c.Expect))
			if gotJSON != wantJSON {
				t.Fatalf("use-case mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

func assertUseCaseError(t *testing.T, err error, want string, expect map[string]interface{}) {
	t.Helper()
	switch want {
	case "missing_variable":
		mv, ok := err.(*MissingVariableError)
		if !ok {
			t.Fatalf("expected *MissingVariableError, got %T (%v)", err, err)
		}
		if v, ok := expect["variable"].(string); ok && mv.Variable != v {
			t.Fatalf("variable %q, want %q", mv.Variable, v)
		}
	default:
		re, ok := err.(*UseCaseError)
		if !ok {
			t.Fatalf("expected *UseCaseError, got %T (%v)", err, err)
		}
		if re.Code != want {
			t.Fatalf("code %q, want %q", re.Code, want)
		}
		if key, ok := expect["key"].(string); ok && re.UseCase != key {
			t.Fatalf("key %q, want %q", re.UseCase, key)
		}
		if p, ok := expect["prompt"].(string); ok && re.Prompt != p {
			t.Fatalf("prompt %q, want %q", re.Prompt, p)
		}
		if list, ok := expect["prompt_names"].([]interface{}); ok {
			if len(list) != len(re.PromptNames) {
				t.Fatalf("prompt_names %v, want %v", re.PromptNames, list)
			}
			for i, v := range list {
				if re.PromptNames[i] != v.(string) {
					t.Fatalf("prompt_names %v, want %v", re.PromptNames, list)
				}
			}
		}
	}
}

// useCaseSelectionToMap projects a useCaseResolution onto the shape use_case.json expects,
// which is also the shape prompt endpoint answers with.
func useCaseSelectionToMap(r *useCaseResolution) map[string]interface{} {
	out := map[string]interface{}{
		"deployment_id":    r.DeploymentID,
		"key":              r.UseCase,
		"revision":         r.DeploymentRevision,
		"kind":             string(r.Kind),
		"params":           r.Params,
		"provider_options": r.ProviderOptions,
		"prompt_names":     r.PromptNames,
		"source":           string(r.Source),
		"warnings":         r.Warnings,
	}
	if out["warnings"] == nil {
		out["warnings"] = []string{}
	}
	if r.Prompt == "" {
		out["prompt"] = nil
	} else {
		out["prompt"] = r.Prompt
	}
	if r.Model == "" {
		out["model"] = nil
	} else {
		out["model"] = r.Model
	}
	if r.ModelID == "" {
		out["model_id"] = nil
	} else {
		out["model_id"] = r.ModelID
	}
	if r.Provider == "" {
		out["provider"] = nil
	} else {
		out["provider"] = r.Provider
	}
	if r.PromptVersionID == "" {
		out["prompt_version"] = nil
	} else {
		out["prompt_version"] = map[string]interface{}{"id": r.PromptVersionID, "number": r.PromptVersionNumber}
	}
	if r.Messages != nil {
		msgs := make([]interface{}, len(r.Messages))
		for i, m := range r.Messages {
			entry := map[string]interface{}{"role": m.Role, "content": m.Content}
			if m.Name != "" {
				entry["name"] = m.Name
			}
			msgs[i] = entry
		}
		out["messages"] = msgs
	}
	if r.Kind == KindText && r.Text != "" {
		out["text"] = r.Text
	}
	return out
}

// ---------------------------------------------------------------------------
// truncation.json

func TestConformanceTruncation(t *testing.T) {
	var suite struct {
		Sampling struct {
			Buckets []struct {
				ID     string `json:"id"`
				Bucket uint32 `json:"bucket"`
			} `json:"buckets"`
		} `json:"sampling"`
		Cases []struct {
			Name   string                 `json:"name"`
			Log    map[string]interface{} `json:"log"`
			Policy *PayloadPolicy         `json:"policy"`
			Config *struct {
				HashEndUser bool `json:"hash_end_user"`
			} `json:"config"`
			Expect struct {
				Log map[string]interface{} `json:"log"`
			} `json:"expect"`
		} `json:"cases"`
	}
	loadConformance(t, "truncation.json", &suite)
	if len(suite.Cases) == 0 {
		t.Fatal("no truncation cases loaded")
	}

	for _, b := range suite.Sampling.Buckets {
		if got := sampleBucket(b.ID); got != b.Bucket {
			t.Errorf("sampleBucket(%q) = %d, want %d", b.ID, got, b.Bucket)
		}
	}

	for _, c := range suite.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			cfg := payloadSettings{Defaults: PayloadPolicy{Mode: PayloadFull, SampleRate: 1, MaxBytes: DefaultMaxBytes}}
			if c.Config != nil {
				cfg.HashEndUser = c.Config.HashEndUser
			}
			got := applyPayloadPolicy(c.Log, c.Policy, cfg)
			gotJSON := string(canonicalJSON(got))
			wantJSON := string(canonicalJSON(c.Expect.Log))
			if gotJSON != wantJSON {
				t.Fatalf("log mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// log_record.json

func TestConformanceLogRecords(t *testing.T) {
	var suite struct {
		Records []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Record      map[string]interface{} `json:"record"`
		} `json:"records"`
		BatchEnvelope struct {
			Request map[string]interface{} `json:"request"`
		} `json:"batch_envelope"`
		FieldRules struct {
			Required []string `json:"required"`
		} `json:"field_rules"`
	}
	loadConformance(t, "log_record.json", &suite)
	if len(suite.Records) == 0 {
		t.Fatal("no golden records loaded")
	}

	validKinds := map[string]bool{"chat": true, "text": true, "embedding": true}
	validSources := map[string]bool{"remote": true, "disk": true, "bundle": true, "manual": true}
	validStopKinds := map[string]bool{"stop": true, "length": true, "tool_call": true, "content_filter": true, "other": true}
	validCostSources := map[string]bool{"provider": true, "catalog": true, "unknown": true}

	var golden []map[string]interface{}
	for _, r := range suite.Records {
		r := r
		golden = append(golden, r.Record)
		t.Run(r.Name, func(t *testing.T) {
			for _, field := range suite.FieldRules.Required {
				if _, ok := r.Record[field]; !ok {
					t.Errorf("required field %q is missing", field)
				}
			}
			id, _ := r.Record["id"].(string)
			if _, ok := LogIDTime(id); !ok {
				t.Errorf("id %q is not a UUIDv7", id)
			}
			if kind, ok := r.Record["kind"].(string); ok && !validKinds[kind] {
				t.Errorf("kind %q is not one of chat/text/embedding", kind)
			}
			if src, ok := r.Record["source"].(string); ok && !validSources[src] {
				t.Errorf("source %q is out of range", src)
			}
			if sk, ok := r.Record["stop_kind"].(string); ok && !validStopKinds[sk] {
				t.Errorf("stop_kind %q is out of range", sk)
			}
			if e, ok := r.Record["error"].(map[string]interface{}); ok {
				kind, _ := e["kind"].(string)
				if !errorKinds[kind] {
					t.Errorf("error.kind %q is out of range", kind)
				}
			}
			if u, ok := r.Record["usage"].(map[string]interface{}); ok {
				if cs, ok := u["cost_source"].(string); ok && !validCostSources[cs] {
					t.Errorf("usage.cost_source %q is out of range", cs)
				}
			}

			// Round-trip: the record builder must reproduce the golden shape
			// byte for byte, once the usage sub-keys a hand-assembled record
			// omitted are written as the explicit nulls the endpoint accepts
			// (field_rules.null_keys).
			rec := recordFromGolden(t, r.Record)
			gotJSON := string(canonicalJSON(sameNumberForm(rec.toMap())))
			wantJSON := string(canonicalJSON(sameNumberForm(withExplicitUsageNulls(r.Record))))
			if gotJSON != wantJSON {
				t.Fatalf("record round-trip mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}

	t.Run("batch_envelope", func(t *testing.T) {
		got := string(encodeBatch(golden))
		want := string(canonicalJSON(suite.BatchEnvelope.Request))
		if got != want {
			t.Fatalf("batch envelope mismatch\n got: %s\nwant: %s", got, want)
		}
	})
}

// sameNumberForm renders every number the same way on both sides of a
// comparison. A Go float64 has lost the literal it was decoded from, so this
// SDK writes 0.000112 where the reference writes 1.12e-4 — the same JSON
// number, and the only difference the round-trip cannot preserve.
func sameNumberForm(v interface{}) interface{} {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, el := range x {
			out[k] = sameNumberForm(el)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, el := range x {
			out[i] = sameNumberForm(el)
		}
		return out
	default:
		return v
	}
}

func withExplicitUsageNulls(record map[string]interface{}) map[string]interface{} {
	out := deepCopyMap(record)
	usage, ok := out["usage"].(map[string]interface{})
	if !ok {
		return out
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cost_usd", "raw"} {
		if _, present := usage[key]; !present {
			usage[key] = nil
		}
	}
	return out
}

// recordFromGolden reads a golden wire record back into a LogRecord, so
// the round-trip exercises the builder rather than a hand-written map.
func recordFromGolden(t *testing.T, m map[string]interface{}) LogRecord {
	t.Helper()
	started, err := time.Parse(time.RFC3339Nano, str(m["started_at"]))
	if err != nil {
		t.Fatalf("started_at %q: %v", m["started_at"], err)
	}
	rec := LogRecord{
		ID:                 str(m["id"]),
		UseCase:            str(m["use_case"]),
		Kind:               Kind(str(m["kind"])),
		Model:              str(m["model"]),
		Status:             str(m["status"]),
		StartedAt:          started,
		LatencyMS:          num(m["latency_ms"]),
		DeploymentID:       str(m["deployment_id"]),
		DeploymentRevision: num(m["deployment_revision"]),
		Prompt:             str(m["prompt"]),
		PromptVersionID:    str(m["prompt_version_id"]),
		ModelID:            str(m["model_id"]),
		Source:             Source(str(m["source"])),
		Provider:           str(m["provider"]),
		ModelUsed:          str(m["model_used"]),
		UpstreamProvider:   str(m["upstream_provider"]),
		FinishReason:       str(m["finish_reason"]),
		StopKind:           StopKind(str(m["stop_kind"])),
		TraceID:            str(m["trace_id"]),
		Sequence:           num(m["sequence"]),
		EndUserRef:         str(m["end_user_ref"]),
	}
	if v, ok := m["params"].(map[string]interface{}); ok {
		rec.Params = v
	}
	if v, ok := m["context"].(map[string]interface{}); ok {
		rec.Context = v
	}
	if v, ok := m["metadata"].(map[string]interface{}); ok {
		rec.Metadata = v
	}
	if v, ok := m["sdk"].(map[string]interface{}); ok {
		rec.SDK = &SDKInfo{Name: str(v["name"]), Version: str(v["version"])}
	}
	if v, ok := m["input"].(map[string]interface{}); ok {
		in := &Input{Text: str(v["text"])}
		if vars, ok := v["variables"].(map[string]interface{}); ok {
			in.Variables = vars
		}
		if msgs, ok := v["messages"].([]interface{}); ok {
			for _, raw := range msgs {
				mm, _ := raw.(map[string]interface{})
				in.Messages = append(in.Messages, Message{Role: str(mm["role"]), Content: str(mm["content"]), Name: str(mm["name"])})
			}
		}
		rec.Input = in
	}
	if v, ok := m["output"].(map[string]interface{}); ok {
		out := &Output{Content: str(v["content"])}
		if calls, ok := v["tool_calls"].([]interface{}); ok {
			out.ToolCalls = calls
		}
		rec.Output = out
	}
	if v, ok := m["error"].(map[string]interface{}); ok {
		rec.Error = &CallError{Kind: str(v["kind"]), Status: num(v["status"]), Message: str(v["message"])}
	}
	if v, ok := m["usage"].(map[string]interface{}); ok {
		u := &Usage{CostSource: str(v["cost_source"])}
		if n, ok := v["input_tokens"]; ok && n != nil {
			u.InputTokens = IntPtr(num(n))
		}
		if n, ok := v["output_tokens"]; ok && n != nil {
			u.OutputTokens = IntPtr(num(n))
		}
		if n, ok := v["cost_usd"]; ok && n != nil {
			f, _ := n.(json.Number).Float64()
			u.CostUSD = Float64Ptr(f)
		}
		if raw, ok := v["raw"].(map[string]interface{}); ok {
			u.Raw = raw
		}
		rec.Usage = u
	}
	return rec
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

func num(v interface{}) int {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil {
		return 0
	}
	return int(i)
}
