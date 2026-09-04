// Command basic shows the whole PromptOn loop: resolve a use case, render the
// pinned prompt, call a provider, log what happened.
//
// The "provider" here is a local stand-in, because PromptOn never calls your
// provider for you and this example should not either. Swap fakeProvider for
// your real client and the rest stays as it is.
//
// Run it against a live project:
//
//	PTN_HOST=http://localhost:4000 PTN_API_KEY=ptn_myproject_… go run ./examples/basic
//
// Or with no server at all, from the snapshot committed next to this file:
//
//	go run ./examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	prompton "github.com/polimo-dev/prompton-go"
)

const useCase = "greeting"

func main() {
	ctx := context.Background()

	cfg := prompton.Config{
		// Host and APIKey fall back to PTN_HOST and PTN_API_KEY.
		Environment: "production",
		// The bundle is the cold-start fallback: committed at migration time and
		// refreshed by the build, it means a process with no disk cache and no
		// network still resolves.
		BundlePath: filepath.Join("examples", "basic", "use-cases.production.json"),
	}
	if os.Getenv("PTN_API_KEY") == "" {
		// Without a key the SDK makes no remote calls at all and works from the
		// bundle, which is exactly what this example needs to run offline.
		cfg.Project = "sdkfixture"
		cfg.DisableDiskCache = true
	}

	client, err := prompton.New(cfg)
	if err != nil {
		log.Fatalf("prompton: %v", err)
	}
	defer func() {
		// Close drains the monitoring-log buffer, best effort.
		if err := client.Close(); err != nil {
			log.Printf("prompton: shutdown flush: %v", err)
		}
	}()

	// Give the first background fetch a moment when a key is configured; a real
	// app would not wait, because the bundle already answers.
	if os.Getenv("PTN_API_KEY") != "" {
		waitForRemote(client)
	}

	variables := map[string]interface{}{"name": "Ada"}

	res, err := client.UseCase(ctx, useCase, prompton.WithVariables(variables))
	if err != nil {
		log.Fatalf("resolve %s: %v", useCase, err)
	}

	fmt.Printf("use case      %s (%s)\n", res.Key, res.Kind)
	fmt.Printf("deployment    %s revision %d\n", res.DeploymentID, res.DeploymentRevision)
	fmt.Printf("prompt        %s (version %d of %v)\n", res.Prompt, res.PromptVersionNumber, res.PromptNames)
	fmt.Printf("model         %s via %s\n", res.Model, res.Provider)
	fmt.Printf("params        %v\n", res.Params)
	fmt.Printf("config from   %s\n\n", res.Source)
	messages, err := res.Messages(ctx, variables)
	if err != nil {
		log.Fatalf("messages: %v", err)
	}
	for _, m := range messages {
		fmt.Printf("  %-9s %s\n", m.Role, m.Content)
	}
	fmt.Println()

	out, err := res.Track(ctx, prompton.CallMeta{
		Variables:  variables,
		Messages:   messages,
		EndUserRef: "user-42",
		TraceID:    "example:1",
		Context:    map[string]interface{}{"language": "en"},
	}, func(ctx context.Context) (*prompton.Result, error) {
		return fakeProvider(ctx, res.Model, messages, res.Params)
	})
	if err != nil {
		log.Fatalf("generation: %v", err)
	}
	fmt.Printf("answer        %s\n", out.Content)

	// Flush is not needed before Close; it is here to show the synchronous path
	// a script or a test would use.
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Flush(flushCtx); err != nil {
		log.Printf("prompton: flush: %v", err)
	}

	if recorded := client.Recorded(); len(recorded) > 0 {
		fmt.Printf("\nno API key configured, so the monitoring log stayed in memory:\n  %s\n", recorded[0]["id"])
	}
}

// fakeProvider stands in for an OpenAI, Anthropic or OpenRouter client. A real
// one would send res.Model, res.Params, res.ProviderOptions and res.Messages,
// with your own provider key.
func fakeProvider(ctx context.Context, model string, messages []prompton.Message, params map[string]interface{}) (*prompton.Result, error) {
	_ = ctx
	_ = model
	_ = params
	last := ""
	if n := len(messages); n > 0 {
		last = messages[n-1].Content
	}
	return &prompton.Result{
		Content:      "Hello! (a stand-in answer to: " + last + ")",
		FinishReason: "stop",
		ModelUsed:    model,
		Usage: &prompton.Usage{
			InputTokens:  prompton.IntPtr(38),
			OutputTokens: prompton.IntPtr(9),
			CostUSD:      prompton.Float64Ptr(0.000112),
			CostSource:   prompton.CostSourceProvider,
		},
	}, nil
}

func waitForRemote(client *prompton.Client) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if client.UseCaseDocumentInfo().Source == prompton.SourceRemote {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
