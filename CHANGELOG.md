# Changelog

All notable changes to this SDK are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## 0.1.0

Initial release.

- `Client` with three tiers of snapshot storage — memory, an atomically written disk cache with an
  ETag sidecar, and a bundle shipped inside the app — loaded in that order and never mixed across
  environments or projects.
- Conditional polling with `If-None-Match` on a 10-second cache TTL, refreshed in the background
  and nudged by the next call. A refresh never blocks or fails a generation: `429` waits out
  `Retry-After`, and `5xx`, timeouts and transport errors back off ×2 up to five minutes while the
  previous document keeps being served.
- Local resolution of snapshot schema v3, byte for byte what `POST /resolve` answers: deployment
  pin, model, layered params and provider options, prompt selection by name with no silent fallback
  to `default`.
- A prompt renderer for the Liquid subset PromptOn allows, with `LintTemplate` and
  `TemplateVariables` mirroring the checks the server runs at commit time.
- `ResolveRemote`, the `POST /resolve` client, cached per use case, prompt and environment, and
  rendered locally.
- Monitoring logs through `Log`, `Flush` and `WithGeneration`: app-generated UUIDv7 ids, size, byte
  and time flush triggers, batches of at most 200 records and 5 MB, one batch per environment,
  partial-acceptance handling, retries with the same ids on `429` and `5xx`, a `413` split in half,
  no retry on any other `4xx`, a bounded queue that drops the oldest, and a best-effort drain on
  shutdown.
- The payload policy applied before a record is queued: sampling on the record id, `none`, `hash`
  and `full` modes, UTF-8-safe head-and-tail truncation, an `end_user_ref` hash option and a redact
  hook that runs last.
- `ModeTest` (no HTTP, logs captured in memory) and `ModeOffline` (disk and bundle only).
- The cross-language conformance suite in `testdata/conformance`, executed by `go test ./...`.
