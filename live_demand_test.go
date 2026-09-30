package prompton

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLiveDemandContract(t *testing.T) {
	path := os.Getenv("PTN_LIVE_CREDENTIALS")
	if path == "" {
		t.Skip("set PTN_LIVE_CREDENTIALS to run the live demand-contract check")
	}
	var creds struct {
		BaseURL        string                 `json:"base_url"`
		APIKey         string                 `json:"api_key"`
		Environment    string                 `json:"environment"`
		Project        string                 `json:"project"`
		PromptKey      string                 `json:"prompt_key"`
		OtherPromptKey string                 `json:"other_prompt_key"`
		Variables      map[string]interface{} `json:"variables"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live credentials: %v", err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatalf("decode live credentials: %v", err)
	}
	if creds.PromptKey == "" {
		creds.PromptKey = "demand_greeting"
	}
	if creds.OtherPromptKey == "" {
		creds.OtherPromptKey = "demand_other"
	}
	if creds.Variables == nil {
		creds.Variables = map[string]interface{}{"name": "World"}
	}
	client, err := New(Config{
		Host:             creds.BaseURL,
		APIKey:           creds.APIKey,
		Environment:      creds.Environment,
		Project:          creds.Project,
		DisableDiskCache: true,
		CacheTTL:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	cold := liveText(t, client, ctx, creds.PromptKey, creds.Variables)
	fresh := liveText(t, client, ctx, creds.PromptKey, creds.Variables)
	other := liveText(t, client, ctx, creds.OtherPromptKey, creds.Variables)
	if cold != "Hello World" || fresh != "Hello World" || other != "Hello World" {
		t.Fatalf("unexpected live renders: cold=%q fresh=%q other=%q", cold, fresh, other)
	}
}

func liveText(t *testing.T, client *Client, ctx context.Context, key string, vars map[string]interface{}) string {
	t.Helper()
	useCase, err := client.UseCase(ctx, key)
	if err != nil {
		t.Fatalf("UseCase(%s): %v", key, err)
	}
	messages, err := useCase.Messages(ctx, vars)
	if err != nil {
		t.Fatalf("Messages(%s): %v", key, err)
	}
	if len(messages) == 0 {
		t.Fatalf("UseCase(%s) returned no messages", key)
	}
	return messages[len(messages)-1].Content
}
