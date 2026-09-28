package prompton

import "testing"

func TestSchemaSevenToolPromptMessagesAndParams(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(`{
		"schema_version":7,
		"project":"p",
		"environment":"staging",
		"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{"temperature":0.2},"input_schema":[]}},
		"deployments":{"chat":{"id":"dep","revision":1,"model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},
		"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","display_name":"GPT","metadata":{},"provider_options":{},"capabilities":["tools"],"status":"active"}},
		"prompt_versions":{"pv":{"id":"pv","prompt_id":"prompt","number":1,"engine":"liquid","tools":{"definitions":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}},"output_schema":{"type":"object"},"output_examples":[{"ok":true}]}],"tool_choice":"auto","parallel_tool_calls":false},"messages":[{"role":"system","content":"Hi {{ name }}"},{"type":"slot","name":"history"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"found"}],"reasoning":{"kept":true}}]}}
	}`))
	if err != nil {
		t.Fatalf("ParseUseCaseDocument: %v", err)
	}

	res, err := resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{
		"name":    "Ada",
		"history": []map[string]interface{}{{"role": "user", "content": "before"}},
	}))
	if err != nil {
		t.Fatalf("resolveSnapshot: %v", err)
	}
	if len(res.Messages) != 4 {
		t.Fatalf("messages len %d", len(res.Messages))
	}
	if res.Messages[0].Content != "Hi Ada" || res.Messages[1].Role != "user" || res.Messages[1].Content != "before" {
		t.Fatalf("slot/render mismatch: %#v", res.Messages)
	}
	if res.Messages[2].ContentValue() != nil || len(res.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant continuation not preserved: %#v", res.Messages[2])
	}
	if _, ok := res.Messages[3].Extra["reasoning"]; !ok {
		t.Fatalf("extra native field not preserved: %#v", res.Messages[3])
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
		"deployments":{"chat":{"id":"dep","revision":1,"model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},
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
		snap, err := ParseUseCaseDocument([]byte(`{"schema_version":7,"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{},"input_schema":[]}},"deployments":{"chat":{"id":"dep","revision":1,"model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","provider_options":{},"capabilities":["tools"]}},"prompt_versions":{"pv":{"id":"pv","engine":"liquid","tools":` + tools + `,"messages":[{"role":"user","content":"hi"}]}}}`))
		if err != nil {
			t.Fatalf("%s parse: %v", name, err)
		}
		if _, err := resolveSnapshot(snap, "chat"); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
}

func TestMessageSlotRequiresVariableList(t *testing.T) {
	snap, err := ParseUseCaseDocument([]byte(`{"schema_version":7,"use_cases":{"chat":{"id":"uc","kind":"chat","default_params":{},"input_schema":[]}},"deployments":{"chat":{"id":"dep","revision":1,"model_id":"model","params":{},"provider_options":{},"prompt_pins":{"default":"pv"}}},"models":{"model":{"id":"model","provider":"openai","model_id":"gpt-4.1","provider_options":{},"capabilities":["tools"]}},"prompt_versions":{"pv":{"id":"pv","engine":"liquid","messages":[{"type":"slot","name":"history"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{})); err == nil {
		t.Fatal("missing slot variable should fail")
	}
	if _, err := resolveSnapshot(snap, "chat", WithVariables(map[string]interface{}{"history": "bad"})); err == nil {
		t.Fatal("non-list slot variable should fail")
	}
}
