package prompton

import (
	"strings"
	"testing"
)

func schema1ToolDocument(tools string, defaultParams string, messages string) string {
	return `{
		"schema_version":1,
		"project":"p",
		"environment":"staging",
		"prompts":{"chat":{"kind":"chat","default_params":` + defaultParams + `,"variables":[]}},
		"deployments":{"chat":{"revision":"v2026.09.30-1","provider":"openai","model":"gpt-4.1","prompt_version":1,"params":{},"provider_options":{}}},
		"models":{"openai:gpt-4.1":{"provider":"openai","model":"gpt-4.1","display_name":"GPT","metadata":{},"provider_options":{},"capabilities":["tools"],"status":"active"}},
		"prompt_versions":{"chat@1":{"prompt":"chat","number":1,"engine":"liquid","tools":` + tools + `,"messages":` + messages + `}}
	}`
}

func TestSchemaOneToolPromptMessagesAndParams(t *testing.T) {
	snap, err := ParsePromptDocument([]byte(schema1ToolDocument(
		`{"definitions":[{"type":"function","function":{"name":"search","parameters":{"type":"object","input_schema":{"type":"object"}}},"output_schema":{"type":"object"},"output_examples":[{"ok":true}]}],"tool_choice":"auto","parallel_tool_calls":false}`,
		`{"temperature":0.2}`,
		`[{"role":"system","content":"Hi {{ name }} {{ history }}"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"found"}],"reasoning":{"kept":true}}]`,
	)))
	if err != nil {
		t.Fatalf("ParsePromptDocument: %v", err)
	}
	res, err := resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{"name": "Ada", "history": "summary"}))
	if err != nil {
		t.Fatalf("resolveSnapshot: %v", err)
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
	tools := res.Params["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	if _, ok := tool["output_schema"]; ok {
		t.Fatalf("output_schema should be stripped from provider params: %#v", tool)
	}
	params := tool["function"].(map[string]interface{})["parameters"].(map[string]interface{})
	if _, ok := params["input_schema"]; !ok {
		t.Fatalf("provider-native tool input_schema must be preserved: %#v", params)
	}
}

func TestSchemaOneToolParamConflict(t *testing.T) {
	snap, err := ParsePromptDocument([]byte(schema1ToolDocument(
		`{"definitions":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"tool_choice":"auto"}`,
		`{"tool_choice":"none"}`,
		`[{"role":"user","content":"hi"}]`,
	)))
	if err != nil {
		t.Fatalf("ParsePromptDocument: %v", err)
	}
	if _, err := resolveSnapshot(snap, "chat"); err == nil {
		t.Fatal("resolveSnapshot should reject conflicting params.tool_choice")
	}
}

func TestSchemaOneToolValidation(t *testing.T) {
	for name, tools := range map[string]string{
		"unknown field":       `{"definitions":[],"bogus":true}`,
		"missing definitions": `{"tool_choice":"auto"}`,
		"bad definition":      `{"definitions":["bad"]}`,
		"missing parameters":  `{"definitions":[{"type":"function","function":{"name":"search"}}]}`,
	} {
		snap, err := ParsePromptDocument([]byte(schema1ToolDocument(tools, `{}`, `[{"role":"user","content":"hi"}]`)))
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
		doc := strings.Replace(schema1ToolDocument(`{"definitions":[]}`, `{}`, `[{"type":"slot","name":"history"}]`), `"engine":"liquid"`, `"engine":"`+engine+`"`, 1)
		snap, err := ParsePromptDocument([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{"history": []map[string]interface{}{{"role": "user", "content": "before"}}}))
		if err == nil || !strings.Contains(err.Error(), "Message slots are not supported; compose conversation history in app code.") {
			t.Fatalf("%s slot error = %v", engine, err)
		}
	}
}

func TestSchemaOneReadableReferencesResolveWithoutResourceIDs(t *testing.T) {
	snap, err := ParsePromptDocument([]byte(`{
		"schema_version":1,
		"project":"p",
		"environment":"staging",
		"prompts":{"support_reply":{"kind":"chat","default_params":{"max_tokens":100},"variables":[{"name":"question","type":"string","required":true}]},"triage":{"kind":"chat","default_params":{},"variables":[]}},
		"deployments":{"support_reply":{"revision":"v2026.10.14-1","provider":"openrouter","model":"google/gemini-3.8-flash","prompt_version":2,"params":{"temperature":0.3},"provider_options":{"allow_fallbacks":false}},"triage":{"revision":"v2026.10.14-2","provider":"anthropic","model":"google/gemini-3.8-flash","prompt_version":2}},
		"prompt_versions":{"support_reply@2":{"prompt":"support_reply","number":2,"kind":"chat","engine":"liquid","messages":[{"role":"user","content":"Reply to {{ question }}"}]},"triage@2":{"prompt":"triage","number":2,"kind":"chat","engine":"liquid","messages":[{"role":"system","content":"Classify only"}]}},
		"models":{"openrouter:google/gemini-3.8-flash":{"provider":"openrouter","model":"google/gemini-3.8-flash","provider_options":{"only":["Google"]}},"anthropic:google/gemini-3.8-flash":{"provider":"anthropic","model":"google/gemini-3.8-flash","provider_options":{"routing":"direct"}}}
	}`))
	if err != nil {
		t.Fatalf("ParsePromptDocument: %v", err)
	}
	support, err := resolveSnapshot(snap, "support_reply", WithVariables(map[string]interface{}{"question": "Ada"}))
	if err != nil {
		t.Fatalf("resolve support_reply: %v", err)
	}
	if support.DeploymentID != "" || support.ModelID != "" || support.PromptVersionID != "" {
		t.Fatalf("schema1 resource ids should stay empty/readable: %+v", support)
	}
	if support.Messages[0].Content != "Reply to Ada" || support.PromptVersionNumber != 2 {
		t.Fatalf("schema1 prompt version did not resolve/render: %+v", support)
	}
	if len(support.Variables) != 1 || support.Variables[0].Name != "question" {
		t.Fatalf("variables metadata did not roundtrip: %#v", support.Variables)
	}
	triage, err := resolveSnapshot(snap, "triage")
	if err != nil {
		t.Fatalf("resolve triage: %v", err)
	}
	if triage.Provider != "anthropic" || triage.ProviderOptions["routing"] != "direct" {
		t.Fatalf("schema1 provider/options did not resolve: %+v", triage)
	}
}

func TestSchemaOneCompositeKeysMustMatchEmbeddedFields(t *testing.T) {
	valid := `{"schema_version":1,"prompts":{"chat":{"kind":"chat","variables":[]}},"deployments":{"chat":{"revision":"v","provider":"p","model":"m","prompt_version":2}},"models":{"p:m":{"provider":"p","model":"m"}},"prompt_versions":{"chat@2":{"prompt":"chat","number":2,"engine":"liquid","messages":[{"role":"user","content":"hi"}]}}}`
	if _, err := ParsePromptDocument([]byte(valid)); err != nil {
		t.Fatalf("valid schema1 parse: %v", err)
	}
	cases := map[string]string{
		"bad prompt version key": strings.Replace(valid, `"chat@2"`, `"other@2"`, 1),
		"bad model key":          strings.Replace(valid, `"p:m"`, `"p:other"`, 1),
		"old metadata":           strings.Replace(valid, `"variables":[]`, `"input_schema":[]`, 1),
	}
	for name, body := range cases {
		if _, err := ParsePromptDocument([]byte(body)); err == nil {
			t.Fatalf("%s should reject malformed schema1 document", name)
		}
	}
}

func TestSchemaOneRejectsLegacyResourceReferences(t *testing.T) {
	cases := map[string]string{
		"prompt id":         `{"schema_version":1,"prompts":{"chat":{"id":"uc","kind":"chat","variables":[]}},"deployments":{},"models":{},"prompt_versions":{}}`,
		"deployment id":     `{"schema_version":1,"prompts":{"chat":{"kind":"chat","variables":[]}},"deployments":{"chat":{"id":"dep","revision":"v","provider":"p","model":"m","prompt_version":1}},"models":{},"prompt_versions":{}}`,
		"prompt version id": `{"schema_version":1,"prompts":{"chat":{"kind":"chat","variables":[]}},"deployments":{},"models":{},"prompt_versions":{"chat@1":{"id":"pv","prompt":"chat","number":1}}}`,
		"model id":          `{"schema_version":1,"prompts":{"chat":{"kind":"chat","variables":[]}},"deployments":{},"models":{"p:m":{"id":"model","provider":"p","model":"m"}},"prompt_versions":{}}`,
		"index response":    `{"schema_version":1,"project":"p","environment":"staging","prompts":[{"key":"chat","kind":"chat"}]}`,
	}
	for name, body := range cases {
		if _, err := ParsePromptDocument([]byte(body)); err == nil {
			t.Fatalf("expected legacy/index rejection for %s", name)
		}
	}
}
