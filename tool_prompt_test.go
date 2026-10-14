package prompton

import (
	"strings"
	"testing"
)

func TestSchemaSevenToolPromptMessagesAndParams(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(`{
		"schema_version":7,
		"project":"p",
		"environment":"staging",
		"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{"temperature":0.2},"input_schema":[]}},
		"deployments":{"chat":{"id":"dep","revision":"v2026.09.30-1","model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},
		"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","display_name":"GPT","metadata":{},"provider_options":{},"capabilities":["tools"],"status":"active"}},
		"prompt_versions":{"pv":{"id":"pv","prompt_id":"prompt","number":1,"engine":"liquid","tools":{"definitions":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}},"output_schema":{"type":"object"},"output_examples":[{"ok":true}]}],"tool_choice":"auto","parallel_tool_calls":false},"messages":[{"role":"system","content":"Hi {{ name }} {{ history }}"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"found"}],"reasoning":{"kept":true}}]}}
	}`))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}

	res, err := resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{
		"name":    "Ada",
		"history": "summary",
	}))
	if err != nil {
		t.Fatalf("resolveSnapshot: %v", err)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("messages len %d", len(res.Messages))
	}
	if res.Messages[0].Content != "Hi Ada summary" {
		t.Fatalf("ordinary history variable did not render: %#v", res.Messages)
	}
	if res.Messages[1].ContentValue() != nil || len(res.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant continuation not preserved: %#v", res.Messages[1])
	}
	if _, ok := res.Messages[2].Extra["reasoning"]; !ok {
		t.Fatalf("extra native field not preserved: %#v", res.Messages[2])
	}
	if res.Params["tool_choice"] != "auto" || res.Params["parallel_tool_calls"] != false {
		t.Fatalf("tool policy params not merged: %#v", res.Params)
	}
	tools := res.Params["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	if _, ok := tool["output_schema"]; ok {
		t.Fatalf("output_schema should be stripped from provider params: %#v", tool)
	}
	if _, ok := tool["output_examples"]; ok {
		t.Fatalf("output_examples should be stripped from provider params: %#v", tool)
	}
}

func TestSchemaSevenToolParamConflict(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(`{
		"schema_version":7,
		"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{"tool_choice":"none"},"input_schema":[]}},
		"deployments":{"chat":{"id":"dep","revision":"v2026.09.30-1","model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},
		"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","provider_options":{},"capabilities":["tools"]}},
		"prompt_versions":{"pv":{"id":"pv","engine":"liquid","tools":{"definitions":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"tool_choice":"auto"},"messages":[{"role":"user","content":"hi"}]}}
	}`))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}
	if _, err := resolveSnapshot(snap, "chat"); err == nil {
		t.Fatal("resolveSnapshot should reject conflicting params.tool_choice")
	}
}

func TestSchemaSevenToolValidation(t *testing.T) {
	for name, tools := range map[string]string{
		"unknown field":       `{"definitions":[],"bogus":true}`,
		"missing definitions": `{"tool_choice":"auto"}`,
		"bad definition":      `{"definitions":["bad"]}`,
		"missing parameters":  `{"definitions":[{"type":"function","function":{"name":"search"}}]}`,
	} {
		snap, err := ParseUseCaseDocument([]byte(`{"schema_version":7,"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{},"input_schema":[]}},"deployments":{"chat":{"id":"dep","revision":"v2026.09.30-1","model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","provider_options":{},"capabilities":["tools"]}},"prompt_versions":{"pv":{"id":"pv","engine":"liquid","tools":` + tools + `,"messages":[{"role":"user","content":"hi"}]}}}`))
		if err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		if _, err := resolveSnapshot(snap, "chat"); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
}

func TestMessageSlotIsRejectedForRawAndLiquidPrompts(t *testing.T) {
	for _, engine := range []string{"liquid", "raw"} {
		snap, err := ParseUseCaseDocument([]byte(`{"schema_version":7,"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{},"input_schema":[]}},"deployments":{"chat":{"id":"dep","revision":"v2026.09.30-1","model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","provider_options":{},"capabilities":["tools"]}},"prompt_versions":{"pv":{"id":"pv","engine":"` + engine + `","messages":[{"type":"slot","name":"history"}]}}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{
			"history": []map[string]interface{}{{"role": "user", "content": "before"}},
		}))
		if err == nil || !strings.Contains(err.Error(), "Message slots are not supported; compose conversation history in app code.") {
			t.Fatalf("%s slot error = %v", engine, err)
		}
	}
}

func TestSchemaEightReadableReferencesResolveWithoutResourceIDs(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(`{
		"schema_version":8,
		"project":"p",
		"environment":"staging",
		"prompts":{
			"support_reply":{"kind":"chat","default_params":{"max_tokens":100},"input_schema":[]},
			"triage":{"kind":"chat","default_params":{},"input_schema":[]}
		},
		"deployments":{
			"support_reply":{"revision":"v2026.10.14-1","provider":"openrouter","model":"google/gemini-3.8-flash","prompt_version":2,"params":{"temperature":0.3},"provider_options":{"allow_fallbacks":false}},
			"triage":{"revision":"v2026.10.14-2","provider":"anthropic","model":"google/gemini-3.8-flash","prompt_version":2}
		},
		"prompt_versions":{
			"support_reply@2":{"prompt":"support_reply","number":2,"kind":"chat","engine":"liquid","messages":[{"role":"user","content":"Reply to {{ question }}"}]},
			"triage@2":{"prompt":"triage","number":2,"kind":"chat","engine":"liquid","messages":[{"role":"system","content":"Classify only"}]}
		},
		"models":{
			"openrouter:google/gemini-3.8-flash":{"provider":"openrouter","model":"google/gemini-3.8-flash","provider_options":{"only":["Google"]}},
			"anthropic:google/gemini-3.8-flash":{"provider":"anthropic","model":"google/gemini-3.8-flash","provider_options":{"routing":"direct"}}
		}
	}`))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}

	support, err := resolveSnapshot(snap, "support_reply", WithVariables(map[string]interface{}{"question": "Ada"}))
	if err != nil {
		t.Fatalf("resolve support_reply: %v", err)
	}
	triage, err := resolveSnapshot(snap, "triage")
	if err != nil {
		t.Fatalf("resolve triage: %v", err)
	}

	if support.DeploymentID != "" || support.PromptVersionID != "" || support.ModelID != "" {
		t.Fatalf("schema8 resource ids should stay empty: %+v", support)
	}
	if support.PromptVersionNumber != 2 || support.Messages[0].Content != "Reply to Ada" {
		t.Fatalf("schema8 prompt version did not resolve/render: %+v", support)
	}
	if support.Provider != "openrouter" || support.Model != "google/gemini-3.8-flash" {
		t.Fatalf("schema8 model did not resolve: %+v", support)
	}
	if support.ProviderOptions["allow_fallbacks"] != false || support.ProviderOptions["only"] == nil {
		t.Fatalf("schema8 provider options did not merge: %#v", support.ProviderOptions)
	}
	if triage.Provider != "anthropic" || triage.ProviderOptions["routing"] != "direct" {
		t.Fatalf("provider-scoped model lookup collided: %+v", triage)
	}
}

func TestSchemaEightPromptVersionPinMustBePositive(t *testing.T) {
	for name, value := range map[string]string{
		"zero":     `0`,
		"negative": `-1`,
		"fraction": `1.5`,
		"bool":     `false`,
	} {
		_, err := ParseUseCaseDocument([]byte(`{
			"schema_version":8,
			"prompts":{"chat":{"kind":"chat"}},
			"deployments":{"chat":{"revision":"v2026.10.14-1","provider":"openrouter","model":"openai/gpt-4o-mini","prompt_version":` + value + `}}
		}`))
		if err == nil {
			t.Fatalf("%s should reject invalid prompt_version", name)
		}
	}
}

func TestSchemaEightCompositeKeysMustMatchEmbeddedFields(t *testing.T) {
	valid := `{
		"schema_version":8,
		"prompts":{"chat":{"kind":"chat"}},
		"deployments":{"chat":{"revision":"v2026.10.14-1","provider":"openrouter","model":"openai/gpt-4o-mini","prompt_version":2}},
		"prompt_versions":{"chat@2":{"prompt":"chat","number":2,"kind":"chat","messages":[{"role":"user","content":"ok"}]}},
		"models":{"openrouter:openai/gpt-4o-mini":{"provider":"openrouter","model":"openai/gpt-4o-mini"}}
	}`
	if _, err := ParseUseCaseDocument([]byte(valid)); err != nil {
		t.Fatalf("valid schema8 parse: %v", err)
	}

	cases := map[string]string{
		"prompt mismatch":   strings.Replace(valid, `"prompt":"chat"`, `"prompt":"other"`, 1),
		"number mismatch":   strings.Replace(valid, `"number":2`, `"number":99`, 1),
		"provider mismatch": strings.Replace(valid, `"provider":"openrouter","model":"openai/gpt-4o-mini"}`, `"provider":"other","model":"openai/gpt-4o-mini"}`, 1),
		"model mismatch":    strings.Replace(valid, `"model":"openai/gpt-4o-mini"}`, `"model":"openai/other"}`, 1),
		"missing version":   strings.Replace(valid, `"chat@2"`, `"chat@3"`, 1),
		"missing model":     strings.Replace(valid, `"openrouter:openai/gpt-4o-mini"`, `"openrouter:openai/other"`, 1),
	}
	for name, body := range cases {
		if _, err := ParseUseCaseDocument([]byte(body)); err == nil {
			t.Fatalf("%s should reject malformed schema8 document", name)
		}
	}
}

func TestSchemaEightRejectsLegacyResourceReferences(t *testing.T) {
	_, err := ParseUseCaseDocument([]byte(`{
		"schema_version":8,
		"prompts":{"chat":{"kind":"chat"}},
		"deployments":{"chat":{"revision":"v2026.10.14-1","provider":"openrouter","model":"openai/gpt-4o-mini","prompt_version":1,"prompt_pins":{"default":"chat@1"}}},
		"prompt_versions":{"chat@1":{"prompt":"chat","number":1,"kind":"chat","messages":[{"role":"user","content":"ok"}]}},
		"models":{"openrouter:openai/gpt-4o-mini":{"provider":"openrouter","model":"openai/gpt-4o-mini"}}
	}`))
	if err == nil || !strings.Contains(err.Error(), "legacy resource references") {
		t.Fatalf("expected legacy resource reference rejection, got %v", err)
	}
}
