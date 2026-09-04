# Changelog

All notable changes to this SDK are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## 0.2.0

Breaking vocabulary cleanup for the schema-v4 use-case document contract.

- Public call flow is `UseCase` → `Messages`/`Text` → `Track`; old resolve/resolution/snapshot
  aliases were removed from the exported API.
- Bundled examples and conformance fixtures now use `use-cases.<environment>.json`,
  `use_case.json`, and `log_record.json`.
- Prompt endpoint decoding follows `key`, `prompt_names`, `source`, `params` and
  `provider_options`, with `/use-cases/{key}/prompt` as the documented path.
- Monitoring sends the `/logs` envelope, and public generation records are now `LogRecord`.
- `ResultFromOpenAI` and `ResultFromAnthropic` normalize provider responses for logs.
- Use-case documents must carry exact integer `schema_version: 4`; legacy `version`, missing,
  older, and newer schema versions are rejected.

## 0.1.0

Initial release.

- `Client` with three tiers of snapshot storage — memory, an atomically written disk cache with an
  ETag sidecar, and a bundle shipped inside the app — loaded in that order and never mixed across
  environments or projects.
- Conditional polling with `If-None-Match` on a 10-second cache TTL, refreshed in the background
  and nudged by the next call. A refresh never blocks or fails a generation: `429` waits out
  `Retry-After`, and `5xx`, timeouts and transport errors back off ×2 up to five minutes while the
  previous document keeps being served.
- Local resolution of snapshot schema v3, byte for byte what `prompt endpoint` answers: deployment
  pin, model, layered params and provider options, prompt selection by name with no silent fallback
  to `default`.
- A prompt renderer for the Liquid subset PromptOn allows, with `LintTemplate` and
  `TemplateVariables` mirroring the checks the server runs at commit time.
- `RemoteUseCase`, the `prompt endpoint` client, cached per use case, prompt and environment, and
  rendered locally.
- Monitoring logs through `Log`, `Flush` and `Track`: app-generated UUIDv7 ids, size, byte
  and time flush triggers, batches of at most 200 records and 5 MB, one batch per environment,
  partial-acceptance handling, retries with the same ids on `429` and `5xx`, a `413` split in half,
  no retry on any other `4xx`, a bounded queue that drops the oldest, and a best-effort drain on
  shutdown.
- The payload policy applied before a record is queued: sampling on the record id, `none`, `hash`
  and `full` modes, UTF-8-safe head-and-tail truncation, an `end_user_ref` hash option and a redact
  hook that runs last.
- `ModeTest` (no HTTP, logs captured in memory) and `ModeOffline` (disk and bundle only).
- The cross-language conformance suite in `testdata/conformance`, executed by `go test ./...`.

Fixed before release, after an adversarial review:

- The canonical JSON encoder substitutes `U+FFFD` for a byte that is not valid UTF-8 instead of
  copying it to the wire. One truncated character from a provider used to make the server refuse
  the whole request body, which dropped up to 200 monitoring logs as a non-retryable `4xx`; the
  batch now stays parseable and only the offending record can be rejected.
- The in-memory log capture of test mode, offline mode and a live client with no API key honours
  `LogMaxBuffer`, dropping the oldest and counting it in `BufferStats().DroppedOverflow`. A missing
  `PTN_API_KEY` is a configuration mistake to ride out, not an out-of-memory kill.
- A local `UseCase` refuses `WithEnvironment` for anything but the client's own environment with an
  `ErrEnvironmentMismatch` `UseCaseError` naming both, instead of silently answering from the
  document it holds. `RemoteUseCase` still honours the option.
- A snapshot that names no environment or project is refused like a mismatched one, so an
  unlabelled hand-assembled bundle cannot boot every process.
- `Log` after `Close` returns `ErrClosed` and counts `BufferStats().DroppedAfterClose` — in every
  mode, whether the record would have been sent or captured — matching what `Flush` already did,
  instead of discarding the record silently.
- The queue bound drops the genuinely oldest record across environments, `BufferStats().Failures`
  reports the worst environment rather than whichever lane finished last, and a batch requeued
  after a failed send restores its bytes to the lane so the byte flush trigger stays accurate.
