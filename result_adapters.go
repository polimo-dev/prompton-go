package prompton

import "encoding/json"

// ResultFromOpenAI converts a typical OpenAI chat response into a PromptOn
// Result. It accepts map-like values or structs that can be marshalled to JSON.
func ResultFromOpenAI(response interface{}) *Result {
	data := mapFrom(response)
	choice := firstMap(data["choices"])
	message := mapFrom(choice["message"])
	usage := mapFrom(data["usage"])
	return &Result{
		Content:          strFrom(message["content"]),
		ToolCalls:        sliceFrom(message["tool_calls"]),
		FinishReason:     strFrom(choice["finish_reason"]),
		Usage:            usageFrom(usage, "prompt_tokens", "completion_tokens"),
		ModelUsed:        strFrom(data["model"]),
		UpstreamProvider: "openai",
	}
}

// ResultFromAnthropic converts a typical Anthropic messages response into a
// PromptOn Result. It accepts map-like values or structs that can be marshalled
// to JSON.
func ResultFromAnthropic(response interface{}) *Result {
	data := mapFrom(response)
	usage := mapFrom(data["usage"])
	content := ""
	for _, item := range arrayFrom(data["content"]) {
		part := mapFrom(item)
		if strFrom(part["type"]) == "text" {
			content += strFrom(part["text"])
		}
	}
	return &Result{
		Content:          content,
		FinishReason:     strFrom(data["stop_reason"]),
		Usage:            usageFrom(usage, "input_tokens", "output_tokens"),
		ModelUsed:        strFrom(data["model"]),
		UpstreamProvider: "anthropic",
	}
}

func mapFrom(value interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]interface{}); ok {
		return m
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

func firstMap(value interface{}) map[string]interface{} {
	items := arrayFrom(value)
	if len(items) == 0 {
		return nil
	}
	return mapFrom(items[0])
}

func arrayFrom(value interface{}) []interface{} {
	if v, ok := value.([]interface{}); ok {
		return v
	}
	return nil
}

func sliceFrom(value interface{}) []interface{} {
	return arrayFrom(value)
}

func strFrom(value interface{}) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func usageFrom(raw map[string]interface{}, inputKey, outputKey string) *Usage {
	if raw == nil {
		return nil
	}
	return &Usage{
		InputTokens:  intPtrFrom(raw[inputKey]),
		OutputTokens: intPtrFrom(raw[outputKey]),
		Raw:          raw,
	}
}

func intPtrFrom(value interface{}) *int {
	switch v := value.(type) {
	case int:
		return &v
	case int64:
		i := int(v)
		return &i
	case float64:
		i := int(v)
		return &i
	default:
		return nil
	}
}
