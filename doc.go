/*
Package prompton is the Go SDK for PromptOn, the control plane for an app's LLM
prompts.

PromptOn is config-fetch, not a proxy. Your app fetches a snapshot of its pins —
per use case and environment, one prompt version set plus one model and its
parameters — renders the pinned prompt with this call's variables, calls the
model provider itself with its own key and its own HTTP client, and sends
monitoring logs back in batches. PromptOn is never in the request path and never
sees your provider key. If PromptOn is down your app keeps running on the last
snapshot it received.

# Quick start

	client, err := prompton.New(prompton.Config{})   // reads PTN_HOST and PTN_API_KEY
	if err != nil {
		return err
	}
	defer client.Close()

	res, err := client.Resolve(ctx, "support_reply",
		prompton.WithVariables(map[string]any{"question": question}))
	if err != nil {
		return err
	}

	out, err := client.WithGeneration(ctx, res, prompton.CallMeta{
		Variables: map[string]any{"question": question},
		Messages:  res.Messages,
	}, func(ctx context.Context) (*prompton.Outcome, error) {
		// Call your provider here, with res.Model, res.Params and res.Messages.
		return &prompton.Outcome{Content: answer, FinishReason: "stop"}, nil
	})

# Resilience

The snapshot lives in three tiers and nothing else: memory, one local file, and
a file bundled into the app. There is no database and no shared cache to run;
instances never coordinate, and ETag polling makes that cheap. Within the cache
TTL every resolve is served from memory with no HTTP call. Past it a conditional
refresh runs in the background, and it never blocks or fails a generation: while
it is in flight, and if it fails, the previous document is served. A rate limit
is waited out, a 5xx is backed off, and a document for another environment or
project is never used.

# Monitoring logs

Three entry points, none of which blocks the provider call: Client.Log queues a
record you built, Client.Flush sends the queue and waits, and
Client.WithGeneration times your provider call and builds the record for you.
Records carry app-generated UUIDv7 ids, so a retried batch is absorbed as
duplicates rather than stored twice.
*/
package prompton
