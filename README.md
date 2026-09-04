# prompton-go

The official Go SDK for [PromptOn](https://app.prompton.ai), the control plane for an app's LLM
prompts.

PromptOn holds, per use case and per environment, one **pin**: a prompt version for each prompt
name, one model, and its parameters. Your app fetches that configuration, renders the prompt with
this call's variables, and **calls the model provider itself** — with its own key and its own HTTP
client. PromptOn is never in the request path, never sees your provider key, and cannot slow your
generation down. What it gets back is **monitoring logs**: one record per model call, batched.

If PromptOn is unreachable, your app keeps running on the last configuration it received.

```
Resolve ──▶ Resolution{Model, Params, Messages, DeploymentID, Prompt, …}
                │
                ├──▶ your provider client, your key
                │
WithGeneration ─┴──▶ one monitoring log, queued and batched
```

## Install

The module is not on a registry yet, so depend on the repository:

```sh
go get github.com/polimo-dev/prompton-go@latest
```

```go
import prompton "github.com/polimo-dev/prompton-go"
```

Requires Go 1.22 or newer. No runtime dependencies: standard library only.

## Quick start

```go
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
	TraceID:   "ticket:" + ticketID,
}, func(ctx context.Context) (*prompton.Outcome, error) {
	// Call your provider here with res.Model, res.Params, res.ProviderOptions
	// and res.Messages. Return what came back.
	answer, usage, err := myProvider.Chat(ctx, res.Model, res.Messages, res.Params)
	if err != nil {
		return nil, prompton.NewCallError(prompton.ErrorKindForStatus(status), status, err.Error())
	}
	return &prompton.Outcome{
		Content:      answer,
		FinishReason: "stop",
		Usage: &prompton.Usage{
			InputTokens:  prompton.IntPtr(usage.In),
			OutputTokens: prompton.IntPtr(usage.Out),
			CostSource:   prompton.CostSourceProvider,
		},
	}, nil
})
```

`examples/basic` is the same thing end to end, runnable with or without a server.

Two calls instead of one, when the prompt and the render happen at different times:

```go
res, _ := client.Resolve(ctx, "support_reply")            // raw templates
messages, _ := res.RenderMessages(map[string]any{"question": question})
```

For a text use case it is `res.Text` and `res.RenderText`; an embedding use case resolves the model
only and has neither.

## Configuration

Precedence is **explicit option → environment variable → default**.

| Option | Env | Default | What it does |
|---|---|---|---|
| `Host` | `PTN_HOST` | `https://app.prompton.ai` | The PromptOn app. The SDK appends `/api/v1` |
| `APIKey` | `PTN_API_KEY` | — | A runtime key, `ptn_<project>_…`. Without one the SDK makes no remote calls and works from disk or bundle, saying so once |
| `Environment` | `PTN_ENVIRONMENT` | `production` | Which environment this process reads. Also the guard on the disk cache and the bundle |
| `Project` | `PTN_PROJECT` | read from the API key | Names the default disk cache file |
| `Mode` | — | `ModeLive` | `ModeLive`, `ModeTest` (no HTTP, logs captured), `ModeOffline` (disk and bundle only) |
| `CacheTTL` | — | `10s` | How long a snapshot is served from memory before a conditional refresh. Also the base of the failure backoff |
| `Timeout` | — | `5s` | Bounds one HTTP request |
| `HTTPClient` | — | a client with `Timeout` | Your own `*http.Client` |
| `DiskCachePath` | — | OS cache dir, named by project and environment | Where the snapshot is mirrored |
| `DisableDiskCache` | — | `false` | Turns the disk tier off |
| `BundlePath` | — | — | A snapshot JSON file shipped inside the app, used when memory and disk are empty |
| `HashEndUser` | — | `false` | Sends `sha256(end_user_ref)` instead of the raw reference |
| `Redact` | — | — | `func(map[string]any) map[string]any`, applied to every record last |
| `PayloadDefaults` | — | `full`, rate `1.0`, 256 KiB | Policy for a use case whose snapshot carries none |
| `LogFlushInterval` / `LogFlushSize` / `LogFlushBytes` | — | `2s` / `100` / `1 MB` | Monitoring-log flush triggers |
| `LogMaxBuffer` | — | `10000` | Queue bound; above it the oldest records are dropped and counted |
| `LogMaxAttempts` | — | `8` | How often one batch is retried before it is dropped and counted |
| `ShutdownTimeout` | — | `5s` | Bounds the best-effort flush `Close` performs |
| `Logger` | — | the standard logger | `func(format string, args ...any)` |

## Resilience

This is the part that matters. A generation must never fail because PromptOn did: the configuration
is stale in the worst case, not absent.

**Three tiers, and nothing else.** Memory, one local file, and a file bundled into the app. No
database, no Redis, no shared cache to operate. Instances never coordinate — each keeps its own
copy, and ETag polling makes that cheap. Several processes on one host may share the disk file:
writes are atomic (temp file, then rename), readers tolerate a concurrent rename, and a corrupt or
partial file is ignored rather than raised.

**Load order at startup**: memory → disk → bundle → remote. `New` never blocks on the network; the
first fetch happens in the background, so the first generation is answered by whatever tier already
had a document. `Resolution.Source` records which one, and it travels with every monitoring log as
`resolution_source`, so a stale deployment is visible in the data.

**Polling.** Within `CacheTTL` every resolve is served from memory with no HTTP call. Past it the
SDK refreshes with `GET /snapshot` + `If-None-Match` — a `304` carries no body and costs nothing.
The refresh runs in the background and is also nudged by the next call, so a scale-to-zero runtime
still refreshes. It never blocks or fails a generation: while it is in flight, and if it fails, the
previous document is served.

**Never used**: a document whose environment or project is not this process's. A staging process
must not boot on a production bundle, and the file records both.

**Building a bundle.** `client.ExportSnapshot("priv/prompton/snapshot.production.json")` writes the
document and its sidecar. Run it in CI on every build and commit the result; one file per
environment, and load the one matching the process. `client.Refresh(ctx)` is the synchronous
"fetch once now" for scripts and one-shot jobs.

### How it fails

| What happens | What the SDK does | What your call sees |
|---|---|---|
| Within `CacheTTL` | Serves memory, no HTTP | The cached configuration |
| `304 Not Modified` | Nothing to parse; the document and ETag stay | The cached configuration |
| `429` on `/snapshot` | Waits out `Retry-After` (else `error.details.retry_after`, else backoff) before contacting the server again | The previous document. No error |
| `5xx`, timeout, DNS, connection refused | Backs off ×2 from `CacheTTL` up to 5 minutes, keeps the previous document, warns once a minute | The previous document. No error |
| PromptOn unreachable at startup, disk cache present | Loads it, keeps polling | `Source: disk` |
| …and no disk cache, bundle present | Loads it, keeps polling | `Source: bundle` |
| …and nothing anywhere | Resolution fails | `ErrNotReady`: "PromptOn is unreachable and nothing is cached" |
| Snapshot for the wrong environment or project | Refuses it and keeps polling | The previous document, or `ErrNotReady` |
| Snapshot `schema_version` 1 or 2 | Refuses it and keeps polling | `*UnsupportedSchemaError` |
| Use case not in the snapshot | — | `ErrUnknownUseCase` |
| Use case with no live deployment | — | `ErrUnresolved` |
| Prompt name the revision does not pin | Never falls back to `default` | `ErrUnknownPrompt`, with `AvailablePrompts` |
| Template needs a variable the call did not send | — | `*MissingVariableError` naming it |
| `429` or `5xx` on `/generations` | Retries the same batch with the same ids, honouring `Retry-After`, backing off 1s ×2 up to 5 min, then drops and counts | Nothing; `Log` already returned |
| `413` on `/generations` | Splits the batch in half and resends both halves | Nothing |
| Any other `4xx` on `/generations` | Drops the batch, counts it, logs once. Never retried | Nothing |
| Log queue full | Drops the oldest and counts it | Nothing |

`ErrUnresolved` and `ErrUnknownPrompt` are bugs in the deployment or in the call — **never** a
signal to reach for a copy of the old prompt string. Fail that call loudly instead.

`client.BufferStats()` reports what was sent and every category of drop.

## Monitoring logs

Three entry points, none of which blocks the provider call:

- **`client.Log(record)`** queues one record you built yourself and returns. It requires
  `UseCase`, `Model`, `Status` and `StartedAt`, and fills in the `ID` (a UUIDv7), the `SDK` name and
  version, and — when you pass a `Resolution` — the deployment, prompt, model and
  `resolution_source` fields.
- **`client.Flush(ctx)`** sends the queue now and waits. For shutdown, tests and scripts.
- **`client.WithGeneration(ctx, res, meta, fn)`** runs your function, measures the latency, builds
  the record and queues it. It returns exactly what your function returned; an error propagates
  unchanged after being logged, and a panic is logged as an `app` error and re-panics.

Records are batched (≤ 200 per request, ≤ 5 MB), one batch per environment, and carry
app-generated **UUIDv7** ids — so a retried batch is absorbed as duplicates rather than stored
twice. Accepted records are never resent; per-record rejections are read from the response and
counted.

`Close()` drains what is queued, best effort.

### The record

| Field | Type | Notes |
|---|---|---|
| `ID` | string | UUIDv7, the idempotency key. Filled in when empty |
| `UseCase` | string | Required |
| `Model` | string | Required — the provider model string that was requested |
| `Status` | string | Required — `StatusOK` or `StatusError` |
| `StartedAt` | time.Time | Required. Defaults to now; rejected if more than 5 minutes ahead or 7 days behind |
| `Kind` | Kind | `chat` (default), `text`, `embedding` |
| `Resolution` | *Resolution | Fills every resolution field below that you left empty |
| `DeploymentID`, `DeploymentRevision`, `Prompt`, `PromptVersionID`, `ModelID` | | Which pin produced the call |
| `ResolutionSource` | Source | `remote`, `disk`, `bundle`, `manual` |
| `Provider`, `ModelUsed`, `UpstreamProvider` | string | Who actually served it |
| `Params` | map | The parameters sent |
| `Input` | *Input | `Variables`, `Messages`, `Text` |
| `Output` | *Output | `Content`, `ToolCalls` |
| `FinishReason` | string | The provider's raw value |
| `StopKind` | StopKind | `stop`, `length`, `tool_call`, `content_filter`, `other`; derived from `FinishReason` when empty |
| `Error` | *CallError | `Kind` (`http_4xx`, `http_5xx`, `rate_limited`, `timeout`, `transport`, `parse`, `app`), `Status`, `Message` |
| `Usage` | *Usage | `InputTokens`, `OutputTokens`, `CostUSD`, `CostSource`, `Raw` |
| `LatencyMS` | int | Measured for you by `WithGeneration` |
| `TraceID`, `Sequence`, `EndUserRef` | | Correlation |
| `Context` | map | Free-form tags. ≤ 2 KB or the server rejects the record |
| `Metadata` | map | Free app data. ≤ 4 KB or the server rejects the record |
| `Environment` | string | Overrides the client's environment for this record |

Send failures as well as successes: error rates and truncation rates are meaningless without them.
And do not log secrets — no provider keys, no `PTN_API_KEY`, no user PII beyond `EndUserRef`.

### Payload policy

Before a record is queued the SDK applies the use case's `payload_policy` from the snapshot, so raw
text never travels further than the policy allows. `mode: none` drops the input and output;
`mode: hash` replaces them with a `{sha256, bytes, hashed}` digest the server understands;
`mode: full` truncates to the same limits the server re-checks, keeping the head and the tail and
never splitting a UTF-8 character. Sampling is a pure function of the record id, so a resend makes
the same decision — and errors and `length` truncations are always kept.

`Redact` runs last, after truncation, on every record.

## Testing

```go
client, _ := prompton.New(prompton.Config{Mode: prompton.ModeTest})
_ = client.SetSnapshot(snapshotJSON)   // or SetSnapshotFile

res, _ := client.Resolve(ctx, "support_reply", prompton.WithVariables(vars))
// … exercise your code …

for _, rec := range client.Recorded() {
	if rec["status"] != "ok" {
		t.Fatalf("unexpected record: %v", rec)
	}
}
```

`ModeTest` makes no HTTP calls at all and captures monitoring logs in memory in the shape they
would have been sent. `ModeOffline` resolves from disk and bundle only and captures logs the same
way — useful for CI and for working on a train.

## Conformance

`testdata/conformance/` is the cross-language contract every PromptOn SDK reproduces: prompt
rendering, resolution, monitoring-log truncation, `stop_kind` normalisation and golden records.
`go test ./...` runs every case. When two SDKs disagree about how a prompt renders or how a log is
truncated, an app that talks to PromptOn from two languages gets two different answers; these files
are what prevent that.

The prompt template engine is the Liquid subset PromptOn allows: `{{ output }}`, `for` (with
`else`, `break`, `continue`, `forloop.*`), `if`/`elsif`/`else`, `unless`, `assign`, and the filters
`size`, `join` and `default`. Anything else is a parse error. `prompton.LintTemplate` and
`prompton.TemplateVariables` expose the same checks the server runs when a prompt version is
committed.

## License

Copyright 2026 Polimo

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).

PromptOn is a trademark of Polimo. The license does not grant permission to use the PromptOn name
or logo; forks and derived services must use a different name.
