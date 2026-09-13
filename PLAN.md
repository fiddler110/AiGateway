# AiGateway (Go) — Build Plan

Status snapshot: 2026-09-12. This is a from-scratch Go rewrite of `../PyAiGateway`,
a self-hosted, provider-agnostic AI proxy gateway for home labs (DLP, cost
control, resilience for outbound AI traffic). The Python version is the
reference for behavior and config shape; this plan tracks where the Go port
already matches it, where it deliberately deviates, and what's still open.

Numbering is `P<phase>.<item>`. Mark an item done in place (`[x]`) rather than
deleting it, so this stays a record of what shipped and why, not just a todo
list — same rationale as the Python repo's `roadmap.md`. Don't renumber
existing items; append new ones at the end of their phase.

---

## Current focus

*Handoff note for the next session. Overwrite this section (don't append to it)
at the end of any session that leaves work unfinished.*

- **Last updated:** 2026-09-13
- **State:** all of Phase 0b except the open P0.19 follow-ups is on `main`.
  On 2026-09-13, at the user's request, `m1/stream-dlp` (P0.3–P0.18,
  including this session's P0.5, P0.15, P0.17, P0.18, P0.19 bullets and
  P0.14 decisions) was fast-forwarded into `main` and both were pushed,
  without a PR. Each item has regression tests; the handler and config ones
  were shown failing against the pre-change commit in a scratch worktree
  (the health tests can't compile there). `go build`, `go vet`,
  `go test -count=1 ./...` pass. **`-race` has never run on this code** (no
  cgo on this machine, and WSL has gcc but no Go).
- **First thing next session:** run `go test -race ./...` in CI, or in WSL
  after installing Go there. P0.11, the half-open probe test, and the new
  health checker are concurrency code that is now on `main` unraced.
- **Next up:** the rest of the P0.19 batch (`middleware_config.<name>`
  unknown keys, `cooldown_seconds: 0` and uncapped retries, decoder error
  text, tool-call `name`/`id` scanning, buffered error-event accounting,
  pre-pipeline audit, health checks on a merged `Manager`, a warning when
  response DLP overrides `stream_buffer: false`). Then M1's remaining P3.1.
- **In progress:** none.
- **Breaking changes on this branch:** unknown YAML keys are rejected.
  `trust_proxy_headers: true` is rejected in favour of `trusted_proxies`;
  `false` only warns. `auth_key: changeme` and a user `gateway_key: changeme`
  are rejected. `auth_key` (or `AIGATEWAY_AUTH_KEY`) together with `users`
  is rejected. `health_path` must start with `/`. `api_format`
  anthropic/gemini are rejected until P1.1/P1.2. `cache.enabled` now
  defaults to `false`. The blank template matches. Clients: streams with
  `secrets_scanner`, `pii_redactor`, or `content_policy` enabled are always
  buffered, and a stream that fails before any byte gets a 503/504 JSON
  error instead of a 200 with an error event.
- **Open questions for the user:** none. All were answered on 2026-09-13
  and recorded in their items: P0.17 (force buffered), P0.19 (reject
  `auth_key` with `users`; keep the 1024-char upstream 4xx relay), P0.14
  (warn on `trust_proxy_headers: false`; keep `/health/detail`; keep SSE
  error events for blocks after a stream's 200), P4.15 (keep the 502 with
  an explicit message), P4.16 (keep one chunk per choice).
- **Test note:** handler harness configs still set `retry_attempts: 0`.
  Removing it would bring back the default 2 retries with real sleeps, so
  leave it. Config loading rejects unknown keys and `middleware_config`
  entries for unlisted middleware, so new test YAML must be exact. A test
  config that enables health checks and sets `health_path` starts a real
  background checker; `testutil.NewGateway` closes it on cleanup through
  `AppState.Close`. `gofmt -l .` lists CRLF-only files on this Windows
  checkout (`core.autocrlf=true`); run it on changed files with CRs
  stripped instead. Code edits with multi-line shell heredocs containing
  apostrophes failed to parse in this harness; edit specs applied by a
  script file worked.
---

## How to use this plan

**Definition of done** (applies to every item, borrowed from the Python
roadmap's closing standard):

1. Code merged with tests that would have failed before the change.
2. `go build ./...`, `go vet ./...`, and `go test -race ./...` pass (the race
   detector needs cgo, so on Windows run it in WSL or CI — see P3.2).
3. Where the item is *behavioral* (anything a client can observe), it is
   exercised end-to-end through `httptest` against a fake upstream (P3.5), not
   only unit-tested in isolation.
4. Any new config field is added to `config/aigateway.blank.yaml` with a
   comment, validated in `config.Validate`, and — if it can hold sensitive
   material — typed `secret.String` (P4.7).
5. The item's checkbox is ticked here with a one-line "verified by" note.

**Sequencing.** Work Phase 0b first: those are defects in code that is
already marked done, and several of them (P0.1, P0.2, P0.4) break the
basic request path that every later phase builds on. After that, the
recommended order is:

| Milestone | Items | Outcome |
|-----------|-------|---------|
| M1 — Trustworthy core | P0.1–P0.14, P3.5, P3.1 (upstream + handlers) | Current feature set actually works and is covered end-to-end |
| M2 — Operable | P1.3, P1.5, P1.6, P1.8, P1.9, P4.9, P4.14 | Reloadable, observable, auditable single instance |
| M3 — Provider parity | P1.1, P1.4, P1.7, P1.2 | Anthropic/Gemini upstreams, dashboard |
| M4 — Shippable | P3.2–P3.4, P3.6, P4.4, P4.10 | CI-gated, containerized, systemd-installable |
| M5 — Scale-out | P2.1–P2.4 | Caching, Redis-backed multi-replica |

Phase 4 items are cross-cutting constraints; land each alongside the phase
item it modifies rather than as a separate batch.

---

## Phase 0 — Core skeleton (DONE, with corrections)

Already built and present in the tree, verified by reading the source (not
just file existence):

- [x] **Config** (`internal/config`): YAML schema (`schema.go`), safe defaults
  (blank upstreams/routes ⇒ every request 400s with an actionable message,
  never a silent route), referential-integrity validation (`validate.go`:
  routes/default_upstream/semantic-cache upstream must all resolve to a
  declared upstream), fnmatch-style glob model routing (`ResolveModelRoute`).
- [x] **Secrets discipline**: `internal/secret.String` wraps anything that must
  never be logged — implements `slog.LogValuer` and `Stringer` to print
  `[REDACTED]` on any accidental log/format call. Upstream provider keys are
  never held in config structs; only the *env var name* is, resolved fresh
  from the process environment at request time.
- [x] **Auth** (`internal/server/auth.go`): three-tier — per-user table (sole
  mechanism if any user exists, constant-time compare across **all** users,
  no early exit, to avoid a timing side channel on which user matched) →
  single shared `auth_key` (constant-time) → open/anonymous with advisory
  `x-client-id`. Source IP resolution ignores `X-Forwarded-For` unless
  `trust_proxy_headers` is explicitly set (prevents rate-limit bypass via
  spoofed headers).
- [x] **Pipeline** (`internal/pipeline`): ordered middleware chain, fail-open
  vs fail-closed per entry, request/response phases run in the *same*
  configured order (not reversed) so `context_pseudonymizer` can reverse its
  own substitutions in the response phase. Three run modes: `Run`,
  `RunResponse` (blocks), `RunResponseAccountingOnly` (used once bytes are
  already on the wire — can flag but never block).
- [x] **Nine middleware ports**, matching the Python reference's behavior:
  `audit_log`, `content_policy`, `cost_tracker`, `pii_redactor`,
  `context_pseudonymizer` (IPv4/v6/CIDR, hostnames, paths, passwords,
  connection creds, session-persistent per client), `rate_limiter`,
  `token_rate_limiter`, `secrets_scanner` (prefix patterns **plus** Shannon-
  entropy fallback for unstructured secrets, ported from the Python side's
  P3.2), `token_counter`. Registry (`internal/middleware/registry.go`)
  resolves middleware by name at config-load time — an unknown name is a
  config error, not a Python-style dynamic-import failure at runtime.
- [x] **Provider translation boundary** (`internal/provider`): a
  `Translator`/`StreamTranslator` interface so middleware, caching, and
  handlers only ever see the canonical OpenAI chat shape; only `openai`
  (identity) is implemented so far. `anthropic`/`gemini` are anticipated in
  the config schema and blank template but not yet implemented (Phase 1).
  Note: `StreamTranslator` is defined but **not yet called** anywhere — the
  streaming handler forwards upstream bytes raw (see P0.13).
- [~] **Upstream manager** (`internal/upstream`): circuit breaker
  (CLOSED/OPEN/HALF-OPEN with single-probe semantics), linear-backoff retry
  within an upstream, fallback across a route's ordered upstream list, and a
  deliberate non-streaming-vs-streaming distinction on *when* success is
  recorded (streaming records success on headers-received, before any body
  byte, since a partial stream can't be safely retried).
  **Correction:** background health checks are *not* implemented.
  `HealthState` exists but `RecordCheck` has no caller and the `health.go`
  referenced in `circuit.go`'s comments does not exist, so every upstream is
  permanently "healthy" despite `health_check.enabled: true` being the
  default (P0.5). `retry_delay_seconds` is also ignored (P0.6), and
  non-streaming requests currently cannot read upstream bodies at all (P0.1).
- [~] **Handlers**: `POST /v1/chat/completions` (streaming + non-streaming,
  buffered vs. passthrough stream modes — buffered is forced whenever
  `secrets_scanner` is active, matching the Python reference's P3.1 finding
  that flag-after-leak is unacceptable for credentials), `GET /v1/models`,
  `GET /health`. Request body size capped (`max_request_bytes`).
  **Correction:** see P0.2–P0.4 and P0.7–P0.9 for status-code, response-DLP,
  and error-envelope defects.
- [x] **Hot-reload primitive**: `server.Server` holds an `atomic.Pointer[AppState]`;
  swapping in a new `AppState` is a single atomic store, so no request ever
  observes a torn old-pipeline/new-upstream-config combination. The *watcher*
  that calls `Swap` on a config-file change is not wired yet (Phase 1).
- [x] Table-driven unit tests exist for: `contentpolicy`, `costtracker`,
  `piiredactor`, `pseudonymizer`, `ratelimiter`, `secretsscanner`,
  `tokencounter`. All pass as of 2026-09-12 (`go build`/`go vet` clean).

**Known gaps in Phase 0 work** (not regressions, just not started):
`tokenratelimiter`, `audit`, `chatmodel`, `config`, `pipeline`, `provider/openai`,
`secret`, `upstream`, `server`, and `handlers` have no tests. `web/` is an
empty placeholder directory. `go.mod` has exactly two dependencies (`chi`,
`yaml.v3`); the stray `// indirect` markers were removed by `go mod tidy` on
2026-09-12. Keep the dependency surface this small deliberately (see P4.10).

---

## Phase 0b — Defects and hardening in shipped Phase 0 code

Found by a source review on 2026-09-12. These are ordered by severity; the
first four break normal traffic and should be fixed before any Phase 1 work.
Each should land with a regression test.

- [x] **P0.1 — Non-streaming upstream bodies are read after their context is
  cancelled.** *Done 2026-09-12: `forward` now reads the body inside its
  timeout context and returns status/header/body. Verified by:
  `upstream.TestSendReadsBodyThatArrivesAfterHeaders` and
  `handlers.TestChatNonStreamingBodyAfterHeaders` (both fail with `context
  canceled` when the read is moved after `cancel()`).* `upstream.forward` (`forward.go:79-87`) creates a
  `context.WithTimeout` and `defer cancel()`s it, then returns the
  `*http.Response`; `Manager.Send` reads the body afterwards
  (`readAndClose`). Cancelling a request's context aborts its body read, so
  any response whose body isn't already fully buffered fails with
  `context canceled` — which is then counted as an upstream **failure**,
  retried, and eventually trips the circuit breaker on a healthy upstream.
  Reproduced in isolation (headers flushed, body sent 50ms later → `read 0
  bytes, err=context canceled`). **Fix:** read the body inside `forward`
  before returning (return `[]byte` + status + headers), or hand the cancel
  func to the caller to invoke after the body is closed. **Test:** fake
  upstream that flushes headers then delays the body.
- [x] **P0.2 — Upstream HTTP status is discarded; 4xx errors reach clients as
  200.** *Done 2026-09-12: `Send` returns `upstream.Result` and `SendStream`
  returns `upstream.StreamResult`, both carrying status + headers; 4xx is not
  retried or counted against the breaker; 1xx/3xx now count as failures like
  5xx. `handlers.writeUpstreamError` relays the status with the gateway
  envelope, taking only a ≤1KiB message from the upstream body (OpenAI,
  Ollama, and bare-`message` shapes; non-JSON bodies aren't reflected),
  passes `Retry-After` on 429, and skips the response pipeline. Streaming 4xx
  is sent as a JSON error, not a 200 SSE stream. 401/403/407 map to 502, see
  P4.15. Verified by: `upstream.TestSendReturnsClientErrorStatus`,
  `TestSendStreamReturnsClientErrorStatus`, and
  `handlers.TestChatRelaysUpstreamClientErrors` (10 subtests: stream and
  non-stream; all fail with the handler relay disabled).* `Manager.Send` returns only `(body, usage, servedBy, err)`, and
  `handlers.Chat` always writes the body with an implicit 200
  (`chat.go:110-111`). An upstream 400/401/404/429 therefore arrives at the
  client as a 200 whose body is an error object — clients that retry on 429
  or surface auth errors will misbehave. The streaming path has the same
  issue: a 4xx from `SendStream` is forwarded as a `200 text/event-stream`.
  **Fix:** return the upstream status (and `Retry-After` for 429) from both
  `Send` and `SendStream`; pass 4xx through with the upstream's status and a
  normalized OpenAI error envelope; skip the response DLP pipeline for
  non-2xx bodies (it currently runs over error JSON).
- [x] **P0.3 — Buffered streaming discards the response pipeline's rewritten
  text.** *Done 2026-09-12: buffered mode collapses the upstream stream to
  one message per choice (`handlers/streambuffer.go`), runs the response
  pipeline over each choice's content as the non-streaming path does, and
  re-emits synthesized chunks (role/content/tool_calls, then finish_reason,
  usage, upstream error events, `[DONE]`). Raw upstream bytes are never
  forwarded; undecodable data lines are dropped (see P4.16 for what the
  re-synthesis doesn't preserve). Verified by:
  `handlers.TestChatBufferedStreamAppliesResponseRedaction` (fails on the old
  code: the email reaches the client).* `serveBufferedStream` calls `pipe.RunResponse(ctx, content, gctx)`
  and ignores the returned text, then writes the *original* raw SSE bytes
  (`chat.go:171-184`). Response-phase redaction by `pii_redactor` (and any
  other rewriting middleware) is silently not applied to streamed responses,
  even in the mode that exists specifically to make response DLP
  enforceable. **Fix:** after a non-blocking `RunResponse`, re-emit the
  stream as synthesized OpenAI chunks from the rewritten content (preserving
  `id`/`model`/`finish_reason`/`usage`/tool-call deltas), rather than
  string-substituting into raw bytes. This also resolves P0.4 for buffered
  mode.
- [x] **P0.4 — Reverse pseudonymization is applied to raw JSON wire bytes.**
  *Done 2026-09-12 (`handlers/streamreverse.go`): passthrough decodes each
  chunk and reverses every string delta field and each tool call's
  `arguments` with a single-pass, leftmost-longest matcher. It holds back a
  suffix that is a proper prefix of some fake until the next chunk for that
  field, the choice's finish_reason, `[DONE]`, or EOF. Arguments are JSON
  source, so they're matched and restored JSON-escaped. With no pseudonyms,
  passthrough forwards upstream bytes unchanged. Buffered mode gets content
  reversal from the pipeline and uses the same reverser for other fields.
  `gctx.ReverseSubstitute` removed. Verified by:
  `handlers.TestChatStreamReversesSplitPseudonyms` (every split point, both
  modes; fails on the old code), `TestChatStreamReversesIntoValidJSON`
  (content and split tool arguments, both modes; old code fails with
  "invalid JSON in stream"), `TestStreamReverserSplitsMatchOneShot`,
  `TestPassthroughReverserReleasesHeldText`.* Both stream modes call `gctx.ReverseSubstitute` on raw SSE lines
  (`chat.go:184`, `chat.go:196`). Two failure modes:
  (a) *split tokens* — in passthrough mode a fake value split across two
  `delta.content` chunks (routine with token streaming: `10.` / `0.3.7`)
  is never reversed, so the client sees fake infrastructure values;
  (b) *JSON corruption* — substituting a real value containing `"` or `\`
  (passwords, Windows paths) into a JSON string without escaping produces
  invalid JSON that clients fail to parse. **Fix:** decode each chunk,
  reverse-substitute on decoded string fields only, re-encode. For (a) in
  passthrough mode, keep a per-choice tail buffer of up to
  `maxFakeLen-1` characters that is withheld until the next chunk proves it
  isn't the prefix of a fake. Add tests for both cases.
- [x] **P0.5 — Background health checks don't exist.** *Done 2026-09-13
  (branch m1/stream-dlp): `internal/upstream/health.go`. `app.BuildState`
  starts one checker per `Manager`, and `AppState.Close` (called by the test
  harness and on shutdown; P1.3 must call it on the retired state) cancels
  it and waits for in-flight probes. Each tick probes upstreams
  concurrently: GET `base_url + health_path` with the upstream's auth
  header. Below 500 is healthy, as in the reference. A 5xx, timeout, or
  network error is a failure, and `RecordCheck`'s 3-in-a-row rule applies.
  Deviation: upstreams without `health_path` aren't probed (the reference
  guesses `/health`, `/v1/models`, then the base URL, where a 404 would
  count as healthy anyway). Config warns when checks are on but no upstream
  has a `health_path`, and `Validate` requires it to start with `/`.
  Verified by: `TestHealthChecksMarkUnhealthyAndRecover`,
  `TestHealthCheckVerdicts`, `TestHealthChecksSkipped`,
  `TestHealthChecksStopOnClose` (upstream), `TestValidateHealthPath`.*
  Implement
  `internal/upstream/health.go`: one goroutine per `Manager` generation,
  ticking at `health_check.interval_seconds`, probing each upstream's
  `health_path` (skip upstreams without one) with `timeout_seconds`, calling
  `RecordCheck`. The goroutine's lifetime must be tied to the `AppState`
  generation (a `context.CancelFunc` stored on `Manager`, cancelled on swap
  by P1.3) — otherwise each hot reload leaks a checker. Until this ships,
  `Validate` should warn that `health_check.enabled` has no effect.
- [x] **P0.6 — Retry behaviour ignores config and wastes time.**
  `Manager.backoff` hardcodes `(attempt+1) × 1s` (`manager.go:292`) instead
  of using `retry_delay_seconds`, and it sleeps after the *final* attempt
  before moving to the next fallback upstream. Also: `RetryAttempts` is not
  validated as `>= 0`, and a negative `FailureThreshold`/`0` opens the
  circuit on the first failure. Fix all three; add jitter (±20%) so multiple
  clients don't retry in lockstep.
  *Done: `Manager.backoffDelay` is linear like the Python reference
  (`retry_delay_seconds × (attempt+1)`) with ±20% uniform jitter. Send
  sleeps only when `attempt < retry_attempts`, through an injectable
  context-aware sleeper, and returns the context error at once if the
  request is cancelled mid-backoff (no further attempts or fallback).
  SendStream has no in-upstream retry, so it never sleeps. `validateResilience`
  rejects `retry_attempts < 0`, negative/non-finite `retry_delay_seconds` and
  `cooldown_seconds`, `failure_threshold <= 0`, and non-positive/non-finite
  health-check interval/timeout. Verified by: `TestSendRetryUsesConfiguredDelay`,
  `TestSendBackoffAbortsOnCancel`, `TestValidateResilience` (all fail on the
  pre-fix code: 3.0s elapsed, 4 circuit failures after cancel, 11 bad values
  accepted), plus seam tests `TestSendBackoffDelays`,
  `TestSendNoSleepAfterFinalAttemptBeforeFallback`,
  `TestSendBackoffSleepErrorStops`, `TestSendStreamNeverBacksOff`,
  `TestWithMergedConfigKeepsBackoffHooks`. `-race` not run (no cgo).*
- [x] **P0.7 — Streaming error events are built by string concatenation.**
  *Done: every SSE error event (upstream failure, buffered read failure,
  response-middleware error, block) goes through `writeSSEError`
  (`handlers/clienterror.go`), which JSON-encodes `server.ErrorEnvelope`, the
  same envelope `WriteError` uses, followed by `[DONE]`. Verified by:
  `TestChatUpstreamFailureIsGenericAndValidJSON` (fails without the fix:
  invalid SSE JSON), `TestWriteSSEErrorEscapesMessage`.*
  `chat.go:138` and `chat.go:179` splice `err.Error()` / `gctx.BlockReason`
  into a JSON literal. Any `"` in an error (URL-bearing net errors, upstream
  bodies) produces malformed JSON the client can't parse. **Fix:** a single
  `writeSSEError(w, status, message)` helper using `json.Marshal`.
- [x] **P0.8 — Internal details leak to clients in error messages.** *Done:
  upstream, timeout, and middleware failures return generic messages with
  `(request_id: <hex>)`, and the full error goes to slog under the same ID
  (no bodies), stream and non-stream. The ID is a minimal per-request
  crypto/rand value set in the `x-request-id` header by the chat handler.
  P1.8 still owns real request IDs (middleware, access log, audit). Verified
  by: `TestChatUpstreamFailureIsGenericAndValidJSON` (fails without the fix:
  host:port and `dial tcp` in body, no header).* Non-
  streaming 503s return `err.Error()` verbatim (`chat.go:95`), and
  `SendStream` embeds the upstream's full 5xx response body in its error
  (`manager.go:266`), which is then sent to the client. Go's `net/http`
  errors include the full upstream URL (`Post "http://10.0.0.5:11434/..."`),
  exposing internal topology — the exact class of data
  `context_pseudonymizer` exists to protect. **Fix:** clients get a generic
  message plus a request ID (P1.8); full detail goes to the server log only.
  *Partial (2026-09-12, with P0.2): `SendStream` no longer puts the 5xx body
  in its error (`TestSendStreamServerErrorIsFailure`). Net-error URLs still
  reach clients via `err.Error()`.*
- [x] **P0.9 — Unbounded memory on upstream responses.**
  *Verified by:* `TestChatNonStreamResponseOverLimit`,
  `TestChatStreamResponseOverLimit`, `TestChatStreamOversizedLine`,
  `TestChatStreamTimeout`, `TestChatStreamUpstreamDrop`,
  `TestChatPassthroughStreamDropMidLine`, `TestLimitReader`,
  `TestValidateResponseLimits` (fail without the fix). Defaults:
  `max_response_bytes` 32MiB, `stream_timeout` 600s (float seconds like
  `request_timeout`). Over-limit non-stream is a terminal 502 (no
  retry/fallback, not a circuit failure); stream errors are SSE events
  (502 too large / 504 timeout / 503 read failure).
  Buffered stream mode
  does `io.ReadAll` on the upstream stream (`chat.go:158`) and `Send` does
  `io.ReadAll` on non-streaming bodies, with no limit — a misbehaving or
  compromised upstream (or a local model stuck in a generation loop) can
  exhaust gateway memory. Passthrough mode's `bufio.Scanner` silently stops
  at a 4MB line and the scanner error is never checked. **Fix:** add
  `settings.max_response_bytes` (default e.g. 32MiB) enforced via
  `io.LimitedReader`; check `scanner.Err()` and emit an SSE error event on
  truncation. Also bound total stream *duration* with a new
  `settings.stream_timeout` (streaming currently has no deadline at all;
  `request_timeout` only applies to non-streaming).
- [x] **P0.10 — Pseudonymizer sessions leak real values across clients.**
  *Verified by:* `TestUnauthenticatedSessionsArePerRequest`,
  `TestAuthenticatedSessionsPersistPerIdentity`,
  `TestPersistSessionsForUnauthenticatedOptIn`,
  `TestChatPseudonymSessionNotSharedBySpoofedClientID`,
  `TestChatPseudonymSessionPersistsForUsersTableIdentity`,
  `TestValidatePersistSessionsForUnauthenticated` (fail without the fix).
  Sessions are keyed by `gctx.ClientID` (`pseudonymizer.go:116`). In
  open-auth mode every client is `anonymous` and in shared-`auth_key` mode
  the client ID is the *advisory, spoofable* `x-client-id` header — so any
  client can adopt another's ID and receive a response whose fake values are
  reversed into that other client's **real** IPs, hostnames, and passwords
  (e.g. prompt "repeat back: 192.0.2.14"). This is a cross-client
  information-disclosure path through the flagship DLP feature.
  **Fix:** key sessions by an *authenticated* identity only — the users-table
  username, or for shared-key/open modes a per-request session (no cross-
  request persistence) unless the operator explicitly opts in via
  `context_pseudonymizer.persist_sessions_for_unauthenticated: true` with
  the risk documented. Reverse mapping should also only restore fakes that
  were issued to *this* session.
- [x] **P0.11 — Pseudonymizer session updates race (lost update / fake
  collision).** *Verified by:* `TestConcurrentRequestsAssignBijectiveMap`
  (fails without the fix: duplicate fakes and lost mappings) and
  `TestSessionAssignResaltsCollisions`. Not yet run under `-race` (no cgo on
  the dev machine); CI should run it. `getOrCreate` returns *copies*, the request extends them
  without the lock, and `merge` replaces the stored maps wholesale
  (`session.go:36-77`). Two concurrent requests from one client each assign
  fakes against a stale snapshot, so (a) the later `merge` drops the earlier
  request's mappings, and (b) both can assign the **same fake to different
  real values**, after which reversal restores the wrong real value.
  **Fix:** make assignment atomic — a `session.assign(real) (fake)` method
  that holds the lock while checking and inserting, so collision checks see
  every committed fake. Add a `-race` concurrency test that fires N parallel
  requests with distinct IPs for one client and asserts a bijective map.
  (This operation boundary is also what the P2.2 `Store` interface needs.)
- [x] **P0.12 — Auth table accepts unsafe configurations.** `Validate` does
  not reject: a user with an empty `gateway_key` (an empty presented
  credential then authenticates as that user, `auth.go:50`); two users
  sharing a key (identity then depends on random map iteration order);
  `users[].upstream` naming an undeclared upstream (silently skipped in
  `Send`, surfacing as a confusing "all upstreams unavailable"); or
  `auth_key` still set to the template's `changeme`. Reject all four.
  Separately: `subtle.ConstantTimeCompare` returns early on length mismatch,
  leaking key length — compare `sha256(presented)` against pre-computed
  `sha256(key)` digests instead (fixed-length, still constant-time).
  Verified by: `TestValidateAuthTable` (also empty usernames and names that
  collide after `SanitizeClientID`), `TestBlankTemplateLoads`,
  `TestAuthenticatorStoresKeyDigests`, `TestAuthenticatorDifferentLengthKeys`.
  Digests live in `server.Authenticator`, built once per `AppState` in
  `app.BuildState`.
- [x] **P0.13 — Streaming never goes through the provider translator.** *Branch m1/stream-dlp.*
  `SendStream` translates the *request* but returns the upstream body raw,
  and `serveStream` assumes it is OpenAI-shaped SSE. Harmless while only
  `openai` exists, but P1.1 cannot work without it. Wire
  `Translator.NewStreamTranslator()` into the stream path now (identity for
  openai) so P1.1 is purely additive. Also make `translatorFor` return an
  error for an unknown `api_format` (`manager.go:131` currently falls back
  to openai silently) and reject unknown `api_format`/`auth_type` values in
  `Validate`.
  *Done 2026-09-12: `SendStream` wraps the size-limited body in a line-based
  adapter (`upstream/streamtranslate.go`). Each native line goes to `Feed`,
  output lines are emitted with `\n`, and `Done` runs only on clean EOF. A
  read error drops the partial line and passes through unchanged, so the
  P0.9 limit and `stream_timeout` still work. The interface is unchanged;
  P1.1 only adds a registry entry. `translatorFor` errors on an unregistered
  format (terminal, not a circuit failure). `Validate` rejects unknown
  `api_format`/`auth_type`, and rejects `anthropic`/`gemini` as "not yet
  supported" until P1.1/P1.2. The chat handler's writer wrapper now
  implements `http.Flusher` only when the real writer does. Verified by:
  `TestSendStreamAppliesStreamTranslator`, `TestUnknownAPIFormatIsError`,
  `TestTranslatedStreamIsLineByLine`, `TestTranslatedStreamErrors`,
  `TestChatStreamUsesProviderStreamTranslator`, `TestChatStreamRequiresFlusher`,
  `TestValidateUpstreamFormatAndAuthType`, and the existing
  `TestChatPassthroughStreamUnchangedWithoutPseudonyms` and `TestBlankTemplateLoads`.
  `-race` not run (no cgo).*
- [x] **P0.14 — Smaller correctness items (batch into one change).** *Branch m1/stream-dlp.*
  *All bullets done 2026-09-12; verified by the tests named in each note.
  `-race` not run (no cgo on this machine).*
  *Decisions 2026-09-13 (user): keep the authenticated `/health/detail`
  until P1.6 `/metrics` replaces it. A response-phase block after a
  stream's 200 stays an SSE error event (headers go out early so keepalives
  can be added later). Streams that fail before any byte is sent (every
  upstream unavailable, or `stream_timeout` before upstream headers) now get
  a real 503/504 JSON error, like non-streaming requests. Verified by:
  `TestChatUpstreamFailureIsGenericAndValidJSON`,
  `TestChatStreamTimeoutBeforeHeaders`.*
  - `SourceIP` splits `RemoteAddr` on the last `:` (`auth.go:100`), which
    leaves brackets on IPv6 (`[::1]`); use `net.SplitHostPort`. With
    `trust_proxy_headers`, taking the **leftmost** `X-Forwarded-For` entry
    trusts a client-supplied value even behind a proxy (proxies append);
    replace the boolean with `trusted_proxies: [CIDR...]` and walk XFF
    right-to-left, stopping at the first untrusted hop.
    *Done 2026-09-12: `trusted_proxies` (CIDRs or bare IPs, parsed in
    `Validate`); `trust_proxy_headers: true` is rejected with a pointer to
    `trusted_proxies`. *Changed 2026-09-13 (user decision): `false` now
    loads with a startup warning instead of failing. It already meant
    "ignore X-Forwarded-For" and the old template shipped it, while `true`
    can't be translated without knowing the proxies' IPs. Verified by:
    `TestValidateTrustedProxies`, `TestConfigWarnings`.* An unparseable XFF entry stops at
    the trusted proxy. Verified by: `TestSourceIP` (server),
    `TestValidateTrustedProxies` (config).*
  - `http.Server` has no `ReadHeaderTimeout`, `ReadTimeout`, or `IdleTimeout`
    (`main.go:75`) — slowloris-style connection exhaustion. Set
    `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`, and leave `WriteTimeout`
    unset (streams) in favour of P0.9's `stream_timeout`.
    *Done 2026-09-12: `newHTTPServer` in `cmd/aigateway/main.go`
    (`ReadTimeout` also left unset for large uploads). Verified by:
    `TestNewHTTPServerTimeouts`.*
  - `token_rate_limiter` never evicts idle keys (unlike `rate_limiter`), so
    IP-keyed anonymous clients grow its map forever. Reuse the same cleanup.
    *Done 2026-09-12: same every-500-calls sweep of keys idle >10 min.
    Verified by: `TestTokenRateLimiterEvictsIdleKeys`.*
  - Blocks always return **400** (`chat.go:63`). Rate-limit and budget blocks
    should return **429** with `Retry-After`; content/DLP blocks stay 400.
    Carry the status on `GatewayContext` alongside `BlockReason`.
    *Done 2026-09-12: `GatewayContext.BlockStatus` + `RetryAfter`;
    rate_limiter, token_rate_limiter and cost_tracker (Retry-After = start of
    next UTC month) set 429; `writeBlock` sends whole seconds, min 1. A
    request larger than all of `tokens_per_minute` is a 400: no wait admits
    it, so Retry-After would loop clients. Blocks after a stream's 200
    (buffered response phase) carry the status only as the SSE error event's
    `code`/`type`, no header; limit blocks are request-phase, so streaming
    requests still get a real 429. Verified by: `TestChatBlockStatus`,
    `TestTokenRateLimiterBlockStatus`.*
  - `audit_log` runs in the request phase, so it logs `candidates[0]` rather
    than the upstream that actually served the request, and never records
    status, latency, tokens, or block outcome. Split into a request-phase
    capture and a post-response write (feeds P1.5). It also opens the log
    file on every request and creates it world-readable (`0o644`) despite
    containing source IPs — keep the handle open, use `0o640`.
    *Done 2026-09-12: new `pipeline.Finisher`, run by `Pipeline.Finish` from
    a handler defer on every exit after the pipeline starts (served, blocked,
    middleware error, upstream failure). New `GatewayContext` fields
    `ServedBy` ("" if no upstream answered; `Upstream` starts as
    `candidates[0]`), `Status` (the sent status, or a failed stream's SSE
    error code, via `outcomeRecorder`), `RequestID`, `StartedAt`. Record:
    time, request_id, client_id, authenticated, source_ip, model,
    message_count, stream, upstream, status, latency_ms, provider-reported
    prompt/completion tokens, blocked, block_middleware, block_direction; no
    block reason, bodies, or matched text. File opened lazily once, `0o640`
    (dir `0o750`), dropped and reopened after a write error, closed by
    `Pipeline.Close` (testutil calls it; hot reload must too once it exists).
    Verified by: `TestChatAuditRecordsOutcome`,
    `TestAuditFileOpenedOnceWithRestrictedMode`.*
  - The half-open probe slot is released *before* the failure is recorded in
    `Send` (`manager.go:170-175`), letting a second probe slip through.
    Release after recording. *Code reordered 2026-09-12 during P0.2 (both
    `Send` and `SendStream` now record, then release); the concurrency test
    is still owed.* *Done 2026-09-12: a `CircuitState.onRelease` test hook
    fires a burst of requests at the instant the slot frees, so the old order
    fails deterministically without -race (checked by swapping the lines).
    Verified by: `TestHalfOpenFailingProbeAdmitsOneRequest` (Send and
    SendStream).*
  - `ReverseSubstitute` and `buildSubstituter` re-sort keys and do
    O(keys × text) `strings.ReplaceAll` passes per call — per SSE line in
    passthrough mode. Build a `strings.Replacer` once per request.
    *`ReverseSubstitute` was removed with P0.4; its replacement is built once
    per request and is single-pass. `buildSubstituter` is unchanged and still
    sequential, so a restored real value that contains another fake gets
    substituted again.* *Done 2026-09-12: the single-pass leftmost-longest
    matcher moved from `handlers/streamreverse.go` to `internal/pseudomap`
    and is shared by the handlers' stream reversal and the pseudonymizer's
    forward substitution and response reversal. Verified by:
    `TestBuildSubstituterSinglePass`,
    `TestProcessResponseRestoredValueContainingFake`, existing
    `TestStreamReverser*`.*
  - `pii_redactor` ran the phone pattern before the card patterns; the phone
    pattern has no leading word boundary, so an unseparated card
    `4111111111111111` became `411111[PHONE]` (and an Amex likewise).
    *Found and done 2026-09-12: card patterns now run before phone and SSN.
    Verified by: `TestRedact` (unseparated 16-digit and Amex cases).*
  - `/health` is unauthenticated and returns upstream names plus circuit
    state. Keep liveness open, but move per-upstream detail behind auth
    (`/health` → `{"status":"ok"}`; detail moves to `/metrics`, P1.6).
    *Done 2026-09-12: detail moved to `GET /health/detail`, behind the same
    `Authenticator` as `/v1` (open auth mode leaves it open, like `/v1`),
    so operators keep circuit visibility until P1.6 supersedes it. Verified
    by: `TestHealthEndpoints`.*
  - `config.Load` doesn't reject unknown YAML keys, so a typo like
    `midleware:` silently disables all DLP. Use `yaml.Decoder.KnownFields(true)`
    at the top level, and reject `middleware_config` entries for middleware
    that isn't in `middleware:` (or at least warn).
    *Done 2026-09-12: `KnownFields(true)` covers nested structs; keys inside
    a `middleware_config.<name>` map are still unchecked (each middleware
    reads its own). Unlisted `middleware_config` entries are rejected.
    Verified by: `TestParseRejectsUnknownKeys`,
    `TestValidateMiddlewareConfigNeedsListedMiddleware`,
    `TestBlankTemplateLoads`.*
  - The schema doc comment promises `auth_key` can be overridden via env, but
    `Load` doesn't implement it. Add `AIGATEWAY_AUTH_KEY` (and a
    `gateway_key_env` alternative per user) so the shared secret doesn't have
    to live in a file that tends to get committed.
    *Done 2026-09-12: resolved in `Parse` before `Validate`, so P0.12 checks
    apply to env values. A set-but-empty `AIGATEWAY_AUTH_KEY`, both
    `gateway_key` and `gateway_key_env` on one user, or an unset/empty named
    variable are errors. Verified by: `TestAuthKeyEnvOverride`,
    `TestGatewayKeyEnv`.*
- [x] **P0.15 — Config fields accepted but not honoured.** *Done
  2026-09-13: `Validate` fills `Config.Warnings` (`warnUnhonoured`) and
  `main` logs them at startup. Covered: `cache.enabled`,
  `cache.semantic.enabled`, `redis.enabled`, and `audit_db` /
  `retention_days` when changed from their defaults. `health_check` is
  honoured since P0.5; its warning is now "no upstream has health_path".
  `cache.enabled` now defaults to `false` (template too), so an untouched
  config logs nothing. Verified by: `TestConfigWarnings`,
  `TestBlankTemplateLoads` (asserts no warnings).* Until the owning
  item ships, each of these should produce a startup warning (not silent
  acceptance), so an operator isn't misled into thinking a protection is
  active:

  | Field | Default | Honoured by |
  |-------|---------|-------------|
  | `cache.enabled` | `true` | P2.1 |
  | `cache.semantic.*` | off | P2.4 |
  | `redis.*` | off | P2.3 |
  | `settings.audit_db`, `settings.retention_days` | set | P1.5 |
  | ~~`resilience.health_check.*`~~ | enabled | P0.5 — *honoured since P0.5* |
  | ~~`resilience.retry_delay_seconds`~~ | 1.0 | P0.6 — *honoured since P0.6* |
  | ~~`upstreams.*.api_format: anthropic\|gemini`~~ | — | P1.1, P1.2 — *rejected by `Validate` since P0.13* |

  Consider flipping `cache.enabled` to default `false` when P2.1 lands:
  response caching across clients is a data-sharing decision the operator
  should make explicitly.
- [x] **P0.16 — Response DLP and reversal only cover `content`.** *Done:
  `pipeline.RunResponse` takes `[]ResponseField`; rewriting middleware run
  per field, and `ResponseAccounting` middleware (token_counter,
  cost_tracker) run once per response over non-derived text. The handlers'
  shared `responseText` (non-stream + buffered) collects every message/delta
  string field except role, plus tool-call arguments as decoded JSON
  string/number tokens spliced back re-encoded (the raw source is also
  scanned, and that rewrite is discarded). Reversal for both goes through the
  pipeline over decoded values, and buffered `sse()` no longer reverses.
  Buffered upstream error events become a generic 502 SSE error. Verified by:
  `TestChatResponsePipelineCoversAllModelFields`,
  `TestChatNonStreamReversesPseudonymsInToolArguments`,
  `TestChatResponseAccountingCountedOnce`,
  `TestChatBufferedStreamHidesUpstreamErrorEvents`, `TestRunResponseFields`
  (all fail against the pre-change code).* Found while
  doing P0.3/P0.4. The response pipeline (`secrets_scanner`, `pii_redactor`,
  `content_policy`) runs over message/delta `content` only. Tool-call
  `arguments` and reasoning fields (`reasoning_content`, `reasoning`,
  `refusal`) reach the client unscanned, both non-streaming and in buffered
  stream mode. Non-streaming also doesn't reverse pseudonyms in tool-call
  arguments, though both stream modes now do. **Fix:** scan every
  model-generated string field (arguments decoded as JSON), share one
  reversal path between the non-streaming and streaming handlers, and decide
  how accounting middleware treats the extra passes
  (`CostFinalized`/`TokensFinalized`).
- [x] **P0.17 — Passthrough streams skip response redaction.** *Done
  2026-09-13 (user chose forcing buffered mode over rejecting the config):
  `middleware.ForcesStreamBuffer` forces buffered streaming whenever
  `secrets_scanner`, `pii_redactor`, or `content_policy` is enabled,
  whatever `stream_buffer` says. `context_pseudonymizer` and accounting
  middleware still allow passthrough. The passthrough-bytes test now uses
  `token_counter`, and the CLAUDE.md invariant was updated. Verified by:
  `TestChatResponseDLPForcesBufferedStream` (fails against the pre-change
  code).* Found during
  P0.16. Buffered mode is forced only when `secrets_scanner` is enabled. With
  `pii_redactor` or `content_policy` enabled on their own, passthrough mode
  sends unredacted model text to the client, and the response middleware can
  only flag it afterwards (an existing handler test asserts that behaviour).
  **Fix:** force buffered mode whenever any response-rewriting or blocking
  middleware is enabled, or reject `stream_buffer: false` with them in
  `Validate`. Update the CLAUDE.md invariant and that test to match.
- [x] **P0.18 — Non-JSON 200 bodies skip response DLP.** *Done 2026-09-13:
  `decodeCompletion` accepts only one JSON object whose `choices` is an
  array of objects that each have a `message` object. Anything else, with
  any middleware configured, is logged (upstream and status, no body) and
  answered with a generic 502 "upstream returned an invalid response". With
  no middleware the body is still forwarded as-is, like the reference.
  Verified by: `TestChatNonStreamUnrecognizedBodyFailsClosed` (fails against
  the pre-change code).* Found during P0.16.
  A non-streaming 200 whose body doesn't decode as a chat completion bypasses
  the response pipeline and reaches the client. DLP must fail closed: return a
  generic 502 (P0.8 path) instead of forwarding it.
- [ ] **P0.19 — Follow-ups found while doing P0.6–P0.16 (batch).**
  - ~~A user's `gateway_key` of `changeme` is accepted; only `auth_key` is
    checked (P0.12).~~ *Done 2026-09-13. Verified by:
    `TestValidateAuthTable`.*
  - ~~`auth_key` set together with a users table isn't rejected, and
    `AIGATEWAY_AUTH_KEY` makes that easier to hit by accident (P0.14).~~
    *Done 2026-09-13 (user decision: once users exist, every request must
    belong to a named user): `Validate` rejects the combination, including
    through the env var. Verified by: `TestValidateAuthTable`,
    `TestAuthKeyEnvWithUsersRejected`.*
  - Keys inside `middleware_config.<name>` aren't checked for unknown fields,
    so a typo in a middleware's own settings is silent (P0.14).
  - `cooldown_seconds: 0` passes validation and effectively disables the
    circuit breaker. Retry counts and delays have no upper cap (P0.6).
  - Upstream 4xx relay (P0.2/P4.15) passes up to 1024 chars of the
    upstream's message text to the client, which could carry internal detail.
    *Decision 2026-09-13 (user): keep it as is, preferring clarity over
    obfuscation for now. Revisit if upstream messages turn out to leak
    something.*
  - Invalid request JSON returns the decoder's `err.Error()` to the client.
    It only describes the client's own input, so low risk.
  - Tool-call `name` and `id` aren't scanned by response DLP (P0.16).
  - Buffered streams that end in an upstream error event skip accounting
    (P0.16).
  - Requests rejected before the pipeline runs (401, bad JSON, no route)
    aren't audited (P0.14 audit split).
  - Passthrough mode drops text held back as a possible partial pseudonym
    when the stream ends in an error. That fails safe; note only.
  - A `Manager` from `WithMergedConfig` (P1.3) doesn't start health checks
    by itself; call `StartHealthChecks` on it. Found during P0.5.
  - `stream_buffer: false` is silently overridden when response DLP
    middleware is enabled (P0.17). Consider a startup warning.
  - For later items: hot reload (P1.3) must call `AppState.Close` (it now
    also stops the health checker) on the old
    state (audit file handle) and currently loses all pseudonymizer sessions.
    P1.1 stream translators must emit an OpenAI-style usage chunk, because
    the usage from `StreamTranslator.Done()` is discarded, or `StreamResult`
    needs a usage field. `forward.go` has unreachable anthropic header code
    until P1.1.

---

## Phase 1 — Request-lifecycle parity with the Python reference

Bring the Go gateway to feature parity for the pieces every deployment will
hit immediately.

- [ ] **P1.1 — Anthropic translator.** Port `gateway/anthropic_translate.py`:
  OpenAI-shaped request → Anthropic Messages API (system message hoisted,
  tool_calls ↔ tool_use/tool_result blocks, `thinking` passed through),
  response translated back (thinking blocks preserved as a non-standard
  field rather than dropped), stateful SSE translator for streaming
  (`content_block_start/delta`, `message_delta`, `message_stop` →
  OpenAI chunk shape, including incremental tool-call argument deltas).
  Auth forced to `x-api-key` + `anthropic-version` header regardless of
  configured `auth_type`, `chat_path` defaults to `/messages`.
  *Depends on:* P0.13. *Additional scope beyond the Python port:*
  - `max_tokens` is **required** by Anthropic; synthesize a configurable
    default (`upstreams.*.default_max_tokens`) when the client omits it.
  - Consecutive same-role messages must be merged (Anthropic rejects them);
    multiple `system` messages concatenated; `image_url` data-URL parts →
    `image` source blocks.
  - Map `stop_reason` → `finish_reason` (`end_turn`→`stop`,
    `max_tokens`→`length`, `tool_use`→`tool_calls`).
  - Anthropic `error` SSE events (e.g. `overloaded_error`) mid-stream must
    become an OpenAI-shaped error chunk, and `overloaded_error` on the first
    event should count as an upstream failure for the circuit breaker.
  - Map Anthropic 529 (overloaded) to a retryable failure like 5xx.
  - Usage: `input_tokens` + cache read/creation tokens → `prompt_tokens`, so
    cost tracking isn't under-counted when prompt caching is in play.
  *Tests:* golden fixtures (P3.6) reusing the Python repo's
  `tests/test_anthropic_translate.py` cases so both implementations are held
  to the same mapping.
- [ ] **P1.2 — Gemini translator.** Not present in the Python reference at
  all — new work, not a port. Same `Translator`/`StreamTranslator` shape.
  Scope this after Anthropic; Gemini's function-calling and safety-rating
  shapes are different enough to warrant their own design pass rather than
  reusing the Anthropic translator's structure. Design notes to resolve in
  that pass:
  - Auth via `x-goog-api-key` header, **never** the `?key=` query parameter
    (query strings end up in proxy and access logs).
  - The model name lives in the URL path (`/models/{model}:generateContent`,
    `:streamGenerateContent?alt=sse`), so `ToUpstream`'s `defaultPath` must be
    computed per request — the interface already allows this.
  - Roles: `assistant`→`model`; `system` → `systemInstruction`; tool results
    → `functionResponse` parts keyed by function *name* (Gemini has no tool
    call IDs — synthesize stable IDs on the way back and keep a name map).
  - `finishReason: SAFETY`/`RECITATION` → `finish_reason: "content_filter"`,
    with safety ratings preserved as a non-standard field.
  - Gemini streams complete JSON candidates rather than token deltas; the
    stream translator must diff successive candidates into deltas.
- [ ] **P1.3 — Hot-reload watcher.** Poll `config.yaml`'s mtime (or use
  `fsnotify` — see P4.10 on dependency budget before adding it) every 5-10s;
  on change, build a full new `AppState` (config load → validate →
  middleware build → upstream manager with carried-forward circuit/health
  state via `WithMergedConfig`, which already exists) **off to the side**,
  and only call `Swap` if that entire build succeeds. An invalid edited
  config must never crash the process or tear down the currently-serving
  `AppState` — log the error and keep serving the last-known-good state.
  This is a stricter requirement than "atomic swap" alone: it's "atomic
  swap, and only ever swap to something already known-valid."
  *Additional requirements found in review:*
  - Poll mtime **and** size + content hash — editors that write-then-rename
    and Kubernetes ConfigMap symlink swaps don't always change mtime the way
    a naive check expects. Debounce (require two identical reads ~500ms apart)
    so a half-written file isn't parsed.
  - Also reload on `SIGHUP` (Unix) for explicit operator control.
  - **Middleware state carry-over is currently undefined.** `middleware.Build`
    constructs fresh instances, so a reload today would reset every client's
    rate-limit window, monthly spend, and pseudonymizer sessions (breaking
    reversal for in-flight multi-turn conversations). Decide per middleware:
    carry state forward when the middleware's config is unchanged (hash its
    `middleware_config` entry), reset otherwise. The P2.2 `Store` interface
    makes this natural — the store outlives the pipeline generation.
  - Old generation cleanup: cancel the previous `Manager`'s health-check
    goroutine (P0.5) after swap; in-flight requests keep their snapshot, so
    no drain is needed beyond that.
  - `settings.listen_host`/`listen_port`/TLS changes cannot be applied live;
    log "restart required" and ignore those fields rather than failing the
    reload.
  - Expose `config_generation`, `last_reload_at`, `last_reload_error` in
    `/metrics`.
- [ ] **P1.4 — `/v1/embeddings` and `/v1/completions` passthrough.** Routed
  the same way as chat (auth, model routing, fallback, audit logging) but
  skip the chat-message DLP pipeline since these bodies aren't chat messages
  — matches the Python reference's documented behavior.
  **Deviation to consider:** skipping DLP on these endpoints is a bypass —
  a client can send the same secret-bearing text to `/v1/completions` that
  `/v1/chat/completions` would block. Run `secrets_scanner`, `pii_redactor`,
  and `content_policy` over `input`/`prompt` strings (via a small adapter
  that presents them as a single user message), and make "skip DLP" an
  explicit per-endpoint opt-out rather than the default.
  `context_pseudonymizer` is the exception for embeddings: substituting
  fakes changes the vector, so leave it off there and document why.
- [ ] **P1.5 — Audit persistence.** A `db` package backing an `audit_log`
  table, `token_usage`, `cost_tracking`, and `blocked_events` (`ts`,
  `client_id`, `model`, `middleware_name`, `reason`, `direction` — never the
  matched content itself). Batched writes off the request hot path.
  Auto-retention pruning on startup (`retention_days`, `-1` disables).
  **Driver decision:** use `modernc.org/sqlite` (pure Go, no cgo) rather than
  `mattn/go-sqlite3` — see P4.2, this matters more in Go than it did in
  Python because a static, cgo-free binary is the whole point of shipping Go
  here.
  *Design detail:*
  - One writer goroutine fed by a bounded channel; flush every 1s or 100
    rows, whichever comes first. When the channel is full, **drop and count**
    (`audit_dropped_total` in `/metrics`) rather than block requests — the
    fail-open posture of `audit_log`. Flush on graceful shutdown.
  - SQLite pragmas: `journal_mode=WAL`, `synchronous=NORMAL`,
    `busy_timeout=5000`; a single write connection, separate read pool for
    `/metrics`.
  - Schema migrations as numbered, embedded SQL files (`embed.FS`) with a
    `schema_version` table — no migration library.
  - Add `request_id`, `status`, `latency_ms`, `upstream` (actual), `stream`,
    `cache` (hit/miss) columns to `audit_log`; P0.14's audit split supplies
    them.
  - Prune on startup **and** daily, in batches, so a long-running instance
    doesn't grow unbounded.
  - `cost_tracker` monthly spend is in-memory today and resets on restart —
    which silently resets budgets. Rehydrate current-month spend from
    `cost_tracking` on startup.
  - Create the DB file `0o600`; it holds client IDs and source IPs.
- [ ] **P1.6 — `/metrics` and `/metrics/prometheus`.** Auth-gated (learn from
  the Python reference's own P1.2 finding — it originally shipped these
  unauthenticated). Token counts, costs, cache stats, upstream health,
  recent audit tail, `blocked_events_by_middleware` + recent blocked events.
  *Design detail:* write the Prometheus text exposition format by hand
  (it's a few dozen lines) rather than importing `client_golang` and its
  transitive dependencies (P4.10). Counters/histograms live in an
  `internal/metrics` package using `sync/atomic`. Keep label cardinality
  bounded: `client_id` is operator-controlled in users mode but
  attacker-controlled via `x-client-id` in open/shared-key mode, so per-client
  labels are only emitted in users mode. Minimum series:
  `aigateway_requests_total{upstream,status}`,
  `aigateway_request_duration_seconds` (histogram),
  `aigateway_upstream_circuit_state{upstream}`,
  `aigateway_blocked_total{middleware,direction}`,
  `aigateway_tokens_total{model,kind}`, `aigateway_cost_usd_total{model}`,
  `aigateway_audit_dropped_total`, `aigateway_config_reloads_total{result}`.
  Consider a separate `settings.metrics_key` so a Prometheus scraper doesn't
  need a key that can also send chat traffic.
- [ ] **P1.7 — `/dashboard`.** Self-contained HTML (no CDN deps), unauthenticated
  route serving a static page that itself prompts for a gateway key
  client-side and polls the (auth-gated) `/metrics` endpoint. Port the
  Python reference's `gateway/static/dashboard.html` behavior rather than
  redesigning it.
  *Go specifics:* ship it via `embed.FS` from `web/` (which gives the empty
  `web/` directory its purpose). Serve with a strict
  `Content-Security-Policy: default-src 'self'; script-src 'self'` (no
  inline script — move JS to an embedded file), `X-Frame-Options: DENY`, and
  `Referrer-Policy: no-referrer`. Store the key in `sessionStorage`, not
  `localStorage` as the Python version does, so it doesn't persist on shared
  machines. Every value rendered from `/metrics` (client IDs, model names,
  block reasons) must be inserted via `textContent`, never `innerHTML` —
  `client_id` is attacker-influenced.
- [ ] **P1.8 — Request IDs and access logging.** Generate a request ID per
  request (honour an incoming `X-Request-Id` only when it matches a strict
  pattern and ≤64 chars), return it as a response header, include it in
  every log line, audit row, and client-facing error body (P0.8). Emit one
  structured access log line per request after completion: method, path,
  status, duration, client_id, model, upstream, stream, bytes, blocked
  middleware. Add a panic-recovery wrapper that logs the stack and returns a
  500 envelope with the request ID. Never log request/response bodies.
- [ ] **P1.9 — Graceful shutdown that respects streams.** `main.go` gives
  `Shutdown` 10s, after which in-flight streams are cut mid-response. Make
  the drain timeout configurable (`settings.shutdown_timeout`, default 30s),
  stop accepting new connections immediately, flush the audit writer (P1.5)
  after HTTP drain, and make `/health` return 503 once shutdown begins so a
  load balancer stops routing new traffic.
- [ ] **P1.10 — `stream_options.include_usage` injection.** Streamed OpenAI
  responses only carry a `usage` chunk when the client asks for it, so most
  streamed requests fall back to the chars/4 estimate and cost tracking
  under-counts. When `cost_tracker` or `token_counter` is active, set
  `stream_options.include_usage: true` on outbound openai-format requests,
  and strip the resulting usage-only chunk from the client stream if the
  client didn't request it (so clients that choke on empty `choices` don't
  break).
- [ ] **P1.11 — Anthropic-native ingress (`POST /v1/messages`) — evaluate,
  don't build speculatively.** Several target clients (Claude Code, Anthropic
  SDKs) speak the Anthropic Messages API natively and need
  `ANTHROPIC_BASE_URL`-style ingress rather than an OpenAI endpoint. The
  P1.1 translator is the reverse direction of what this needs, but the
  canonical-shape design means ingress is "Anthropic → canonical" on the way
  in and "canonical → Anthropic" on the way out, reusing the same mapping
  tables. Decide after P1.1 ships whether demand justifies it; record the
  decision here either way.

---

## Phase 2 — Caching and horizontal scale

- [ ] **P2.1 — Exact-match response cache.** In-memory LRU, keyed on
  SHA-256(model + temperature + max_tokens + messages). Bypass via
  `X-Cache-Control: no-cache`. Never caches streaming requests or error
  responses. Stats at `/metrics`.
  **Corrections to the Python key design:**
  - The key must include **every** field that changes the output: `tools`,
    `tool_choice`, `response_format`, `top_p`, `seed`, `stop`, `n`, and
    provider-specific `Extra` fields — hashing only model/temperature/
    max_tokens/messages returns wrong cached answers for tool-calling
    requests. Hash a canonical JSON encoding of the whole request minus
    `stream` and `user`.
  - The key must include the **client identity** (or an explicit
    `cache.scope: global|client` setting, defaulting to `client`). A
    globally shared cache returns one client's response to another — and
    responses may contain data pseudonymized for, then reversed into, the
    original client's real values.
  - Hash the request **after** the request pipeline runs (post-redaction,
    post-pseudonymization), and run the response pipeline on cache hits, so
    DLP and reversal apply identically to cached and live responses.
  - Don't cache when `temperature > 0` unless `seed` is set, by default
    (`cache.cache_nondeterministic: false`).
  - A request that is rate-limited or over budget must still be blocked on
    a cache hit (limiters run before the cache lookup).
- [ ] **P2.2 — `Store` abstraction, decided now, implemented incrementally.**
  Before writing a Redis backend, define one small interface that
  `rate_limiter`, `token_rate_limiter`, `context_pseudonymizer`'s session
  map, and the response cache all use for their shared/mutable state
  (roughly: sliding-window counter ops + a generic get/put/TTL surface).
  Ship the in-memory implementation first (this is what Phase 0's
  middleware already use, informally); adding a Redis implementation later
  becomes additive, not a rewrite of four packages. This is the one
  concrete architecture change from the Python reference's structure: the
  Python code added Redis by threading an optional client through
  `build_pipeline()` after the fact — doing the interface extraction first
  here avoids that retrofit.
  *Proposed shape* (operations are atomic, so the Redis implementation can
  map each to one Lua script / `MULTI`):
  ```go
  type Store interface {
      // Sliding window: prune entries older than window, and if sum+weight <= limit,
      // record weight at now. Returns whether admitted and the time until capacity frees.
      WindowAdmit(ctx context.Context, key string, weight, limit int, window time.Duration, now time.Time) (ok bool, retryAfter time.Duration, err error)
      // Pseudonym sessions: atomically return the existing fake for real, or assign one
      // produced by gen (which receives the set of taken fakes). Refreshes TTL.
      AssignPseudonym(ctx context.Context, session, real string, gen func(taken func(string) bool) string, ttl time.Duration) (fake string, err error)
      Reverse(ctx context.Context, session string) (map[string]string, error)
      Get(ctx context.Context, key string) ([]byte, bool, error)
      Put(ctx context.Context, key string, val []byte, ttl time.Duration) error
      AddFloat(ctx context.Context, key string, delta float64) (float64, error) // cost_tracker spend
  }
  ```
  Note `AssignPseudonym` is the atomic operation P0.11 requires, and
  `AddFloat` brings `cost_tracker`'s monthly spend into scope (the Python
  reference left spend per-process, which under-enforces budgets with >1
  replica). Every method takes `ctx` and returns `error` even in memory, so
  the Redis backend's failure modes are already handled by callers — and
  those callers must respect their middleware's fail-open/closed policy on
  store errors.
- [ ] **P2.3 — Redis-backed `Store` implementation.** Sorted-set sliding
  windows for the two rate limiters, a per-client hash+TTL for the
  pseudonymizer session map, a Redis-backed response cache. Config-gated
  (`redis.enabled`), zero-dependency in-memory path stays the default.
  *Security/ops requirements:* support `rediss://` (TLS) and ACL
  username/password with the password taken from an env var
  (`redis.password_env`), never inline in `redis.url`; the pseudonymizer hash
  holds real secrets, so document that Redis must not be network-exposed and
  consider encrypting session values at rest with a key from env. Use
  server-side time (`TIME`) in the window scripts so replica clock skew
  doesn't skew limits. On Redis unavailability: rate limiters follow their
  fail policy (closed by default), the cache is always fail-open (miss).
  Dependency choice (`redis/go-redis` vs. a minimal RESP client) goes
  through P4.10.
- [ ] **P2.4 — Semantic cache (optional).** Layered in front of the exact
  cache; embeds the prompt via the configured upstream's real
  `/embeddings` endpoint (no bundled local model), cosine-similarity scan
  scoped per-model. Off by default; keep `similarity_threshold` high in the
  shipped template (a near-miss match means a request can be answered from
  another client's cached response).
  *Additional constraints:* scope per-model **and** per-client by default
  (same reasoning as P2.1); embed the **post-DLP** prompt so raw secrets are
  never sent to the embeddings upstream; only embed the last user turn plus
  a hash of the preceding transcript (a full-conversation embedding makes
  every multi-turn request a near-duplicate of the last one); pre-normalize
  vectors so similarity is a dot product; linear scan is fine up to
  `max_entries` ~ a few thousand — document that ceiling rather than adding
  an ANN index. An embed failure is a cache miss, never a request failure.

---

## Phase 3 — Test coverage, CI, deployment

- [ ] **P3.1 — Fill test gaps.** `chatmodel` (message scan/sanitize/transform
  helpers), `config` (validate + glob routing edge cases), `upstream`
  (circuit breaker state machine + retry/fallback against `httptest`
  fakes), `server/auth` (constant-time paths, all three auth tiers),
  `handlers` (end-to-end via `httptest`: blocked requests, streaming buffer
  vs. passthrough mode, timeout handling), `tokenratelimiter` (currently
  untested).
  *Specific cases the review showed are needed:* `ChatRequest`/`ChatMessage`
  JSON round-trip preserves `Extra` byte-for-byte (tool calls, `tools`,
  `response_format`); `globMatch` with pathological patterns (`*a*a*a*…b`)
  — the recursive matcher is exponential, so either bound pattern count/
  length in `Validate` or replace with `path.Match`-style iterative matching;
  circuit breaker half-open single-probe under concurrency; every P0.x
  regression; `secret.String` redaction through `slog` JSON and text
  handlers and `fmt` `%v`/`%+v`/`%#v`.
- [ ] **P3.2 — Concurrency correctness.** Run the full suite under
  `go test -race` in CI as a hard gate — this is Go-specific value the
  Python port couldn't get for free (GIL aside, asyncio has its own races);
  the atomic `AppState` swap and every shared middleware state map are
  exactly the kind of thing `-race` catches early. (`-race` requires cgo; on
  the Windows dev box run it via WSL or rely on the Linux CI job.) Add
  targeted concurrent tests for P0.11 and for reload-under-load (P1.3):
  N goroutines sending requests while another swaps state in a loop.
- [ ] **P3.3 — CI pipeline.** Build, `go vet`, `golangci-lint`, `go test
  -race ./...`, `govulncheck` (see P4.10).
  *Concretely:* GitHub Actions on Linux; `gofmt -l` must be empty (clean
  since `gofmt -w .` on 2026-09-12); golangci-lint with at least `errcheck`, `gosec`,
  `bodyclose`, `contextcheck`, `noctx`, `staticcheck` — `bodyclose` and
  `contextcheck` would have flagged P0.1-class bugs; cross-compile matrix
  (`linux/amd64`, `linux/arm64` for Raspberry Pi homelabs, `windows/amd64`,
  `darwin/arm64`) with `CGO_ENABLED=0` to enforce P4.2; short fuzz runs
  (P4.6, `-fuzztime=30s` per target) on every PR; coverage report (no hard
  threshold initially).
- [ ] **P3.4 — Deployment artifacts.** Multi-stage `Dockerfile` (static
  binary, distroless or scratch final stage — no cgo means this is
  genuinely tiny), `docker-compose.yml` (+ optional `redis` service, same
  shape as the Python reference's), k3s manifests. Port `HOMELAB_DEPLOYMENT.md`
  and `CLIENT_SETUP.md`, adjusted for a single static binary as a first-class
  deployment option (systemd unit) alongside containers — this wasn't
  really viable for the Python version and is a genuine differentiator here.
  *Hardening defaults to ship:* container runs as non-root
  (`distroless/static:nonroot`), read-only root filesystem with a writable
  volume only for `logs/`; systemd unit with `DynamicUser=yes`,
  `ProtectSystem=strict`, `ProtectHome=yes`, `NoNewPrivileges=yes`,
  `PrivateTmp=yes`, `StateDirectory=aigateway`, and keys supplied via
  `LoadCredential=`/`EnvironmentFile=` (mode `0600`); k3s manifest with
  `securityContext` (`runAsNonRoot`, `readOnlyRootFilesystem`,
  `allowPrivilegeEscalation: false`, dropped capabilities), keys from a
  `Secret`, and a `NetworkPolicy` restricting egress to configured
  upstreams. Add a `--check-config` flag (load + validate + build pipeline,
  exit 0/1) for use in CI and before restarts, and a `--version` flag with
  build info embedded via `-ldflags`. Release binaries with checksums
  (GoReleaser or a plain Makefile target — decide under P4.10).
- [x] **P3.5 — Fake-upstream test harness.** *Done 2026-09-12:
  `internal/testutil`. `NewFakeUpstream(t, script...)` provides scripted
  `Response`s (status, headers, header/body delays, per-flush `Chunks`,
  `Disconnect`), request recording, and the builders `OpenAIChat`,
  `OpenAIError`, `OpenAISSE`, `AnthropicMessage`, `AnthropicSSE`,
  `OversizedChat`. `NewGateway(t, yaml)` serves the real `app.NewRouter`.
  Packages that `app` imports (e.g. `upstream`) must use external `_test`
  packages to import `testutil`. Verified by: `testutil` self-tests (script
  order, delays, disconnect truncation) plus the P0.1/P0.2 tests built on it.* A reusable `internal/testutil`
  package: an `httptest.Server` that scripts OpenAI- and Anthropic-shaped
  responses (fixed JSON, SSE sequences with controllable chunk boundaries
  and delays, 4xx/5xx, mid-stream disconnect, slow headers, oversized
  bodies) and records the requests it received. Plus a helper that builds a
  full `AppState` + router from an inline YAML string. This is the
  prerequisite for most P0.x regression tests and for P3.1's handler tests,
  so build it first in M1.
- [ ] **P3.6 — Cross-implementation golden fixtures.** Store translator and
  DLP test vectors as JSON files under `testdata/` (input request, expected
  upstream body, upstream response, expected client response) so the same
  cases can be run against both this repo and PyAiGateway. Divergences are
  then either bugs or documented deviations (listed in Phase 4), never
  accidents.
- [ ] **P3.7 — Load and latency baseline.** A `k6` or plain Go benchmark
  script against a fake upstream measuring gateway-added latency (p50/p99)
  with the full middleware stack, non-streaming and streaming
  time-to-first-byte in both stream modes. Record the numbers here; it's
  the evidence for whether buffered-mode latency (flagged as a possible
  follow-up in the Python roadmap's P3.1) matters in practice, and a guard
  against regressions from P0.4's per-chunk decode/re-encode.

---

## Phase 4 — Architectural and security decisions that deviate from the Python reference

Concrete answers to "are there better architectural/security decisions we
can make" — evaluated against what Go actually buys, not changed for its own
sake. Items here are cross-cutting; each references the phase item(s) it
modifies.

- [ ] **P4.1 — Validate-before-swap on reload (see P1.3).** Stronger than the
  Python reference's "atomic config swap" — that guarantees no torn state,
  this additionally guarantees no *invalid* state is ever swapped in.
- [ ] **P4.2 — Pure-Go SQLite (`modernc.org/sqlite`), not cgo (see P1.5).**
  A cgo dependency reintroduces exactly the cross-compilation/static-binary
  friction Go was chosen to avoid. Enforced by P3.3's `CGO_ENABLED=0`
  cross-compile matrix.
- [ ] **P4.3 — `Store` interface before the Redis backend, not after (see P2.2).**
  Ordering decision, not a new capability — avoids the retrofit the Python
  side did.
- [ ] **P4.4 — Optional built-in TLS listener.** The Python reference
  explicitly punts TLS termination to a reverse proxy (nginx/Caddy) and
  documents that as a limitation. Go's stdlib makes `ListenAndServeTLS`
  essentially free — add `settings.tls.cert_file`/`key_file` as an *optional*
  path for single-binary homelab deployments that don't want to stand up a
  separate reverse proxy, while keeping "behind a reverse proxy" as the
  documented recommended default for anything multi-instance or with real
  cert lifecycle needs (ACME/renewal is still better handled by Caddy).
  *Detail:* `MinVersion: tls.VersionTLS12`; reload cert/key on change via
  `GetCertificate` (so a renewed cert is picked up without restart — this is
  loading, not lifecycle management, so it stays within the non-goal);
  optional `client_ca_file` for mTLS, which is a genuinely stronger auth
  option for machine clients than bearer keys.
- [ ] **P4.5 — Bound the DLP scan surface.** `secrets_scanner`,
  `pii_redactor`, `context_pseudonymizer`, and `content_policy` all run
  attacker-controlled input through regex and entropy scans. Cap the input
  length fed to each scan (already implicitly bounded by
  `max_request_bytes`, but re-check per-message rather than only per-body)
  and add a per-middleware processing deadline via `context.Context` so a
  pathological input can't stall a request indefinitely.
  **Correction (2026-09-12):** classic regex ReDoS (catastrophic
  backtracking) is *not* a risk here — all patterns use Go's stdlib
  `regexp`, which is RE2-based and guarantees linear-time matching. Keep it
  that way: never introduce a backtracking engine (e.g. `dlclark/regexp2`),
  and reject lookaround in operator-supplied `content_policy` patterns by
  virtue of compiling them with `regexp`. The remaining real costs are
  (a) linear-but-large work: patterns × messages × bytes, plus
  pseudonymizer substitution at O(keys × text); (b) allocation from the
  entropy tokenizer on long base64-ish inputs; (c) the exponential recursive
  `globMatch` in config routing (P3.1). Bound those. Note too that the
  deadline must be *checked* inside long loops — `context` cancellation is
  cooperative and a regexp call cannot be interrupted, so cap per-call input
  length rather than relying on the deadline alone.
- [ ] **P4.6 — Fuzz tests for the regex-heavy middleware.** `go test -fuzz`
  is native and cheap; run it against `secretsscanner`, `pseudonymizer`
  (detectors), and `content_policy`'s pattern matching. This is something
  the Python port didn't get for free and is worth doing specifically
  *because* P4.5 identifies these as the attacker-facing parsers.
  *Useful properties to fuzz, beyond "doesn't panic":* pseudonymize →
  reverse is the identity on arbitrary text (catches P0.4/P0.11-class
  bugs); `ChatRequest` unmarshal → marshal → unmarshal is stable;
  `chatmodel.MapMessageScanText` with an identity function leaves the
  message byte-equivalent; SSE chunk re-splitting at arbitrary boundaries
  produces the same reversed output (P0.4a); translators never emit invalid
  JSON.
- [ ] **P4.7 — Keep the "never log a secret" guardrail extensible.** Every
  new config field that can hold sensitive material (a future per-user
  resolved key, a future upstream credential cache, etc.) must use
  `secret.String`, not a bare `string`. Add a CI check (a small grep-based
  test or golangci-lint custom rule) that flags new struct fields named
  like `*key*`/`*secret*`/`*password*`/`*token*` that aren't typed
  `secret.String`, so this stays enforced rather than convention-only.
  *Implementation:* a `go/ast`-based test in `internal/secret` walking all
  packages is simpler than a custom linter and runs in plain `go test`. It
  needs an allowlist for legitimate names (`APIKeyEnv`, `UpstreamKeyEnv`
  hold env var *names*; `MaxTokens`, `EstimatedTokens` are counts). Also
  note the gap `secret.String` can't close on its own: the resolved upstream
  key is a bare `string` passed through `apiKeyFor` → `UpstreamRequest.APIKey`.
  Type that field `secret.String` too, unwrapping only inside
  `buildHeaders`.
- [ ] **P4.8 — Never leak matched content in block reasons.** Already true
  in the current `secretsscanner`/`contentpolicy` code (`"<pattern> pattern
  matched"`, not the match itself) — carry this invariant forward
  explicitly into `/metrics`, `/dashboard`, and `blocked_events` (P1.5/P1.6):
  those surfaces must only ever show the generic reason string, matching
  the Python reference's P2.3 finding. Extend to error paths: P0.8's
  generic client errors, and upstream error bodies (which can echo the
  request back) must not be written to audit rows verbatim.
  `cost_tracker`'s block reason currently includes the client's spend and
  budget figures — acceptable to the client itself, but it must not be
  copied into shared surfaces (`/metrics` recent-blocks table) as-is.
- [ ] **P4.9 — Metrics/dashboard auth from day one.** Build `/metrics` and
  `/dashboard` (P1.6/P1.7) auth-gated from the first commit that adds them,
  rather than shipping open and fixing it later as the Python reference's
  own history (P1.2) shows it did. **Open-auth mode caveat:** with no
  `auth_key` and no users, "auth-gated" is a no-op — so in open mode,
  `/metrics` should be disabled unless `settings.metrics_key` is set
  (P1.6), since it exposes other clients' IDs, source IPs, and spend.
- [ ] **P4.10 — Keep the dependency surface small, and check it.** Currently
  two third-party modules (`chi`, `yaml.v3`). Prefer stdlib
  (`net/http`'s `ServeMux` pattern routing is sufficient for this route
  count as of Go 1.22+; keep `chi` only if a concrete need — e.g. its
  middleware chaining ergonomics — justifies it) over adding routers/
  frameworks. Any new dependency (`fsnotify`, a Redis client, a Prometheus
  client library) should be a deliberate, reasoned addition, and
  `govulncheck` (P3.3) should run in CI from the point any dependency beyond
  the current two is added.
  *Recommendation:* drop `chi` now — `main.go` uses only `Post`/`Get`
  registration, which `http.ServeMux` handles natively (`"POST
  /v1/chat/completions"`), and `go.mod` already targets Go 1.23. Record each
  future dependency decision in this item as a one-line entry:
  `module — why — alternatives considered`. Expected additions:
  `modernc.org/sqlite` (P1.5, accepted by P4.2); Redis client (P2.3, TBD);
  `fsnotify` (P1.3, *rejected in favour of polling* unless polling proves
  inadequate).
- [ ] **P4.11 — Pseudonymizer sessions bound to authenticated identity (see
  P0.10).** A deliberate deviation: the Python reference keys sessions by
  `client_id` too, and therefore has the same cross-client reversal leak in
  shared-key/open modes. Record the fix as a deviation and file it back
  against PyAiGateway. *Go side landed with P0.10 (2026-09-12, branch
  `m1/stream-dlp`): only users-table identities get persistent sessions,
  shared-key/open requests get per-request sessions unless
  `persist_sessions_for_unauthenticated: true`. Still owed: filing it against
  PyAiGateway.*
- [ ] **P4.12 — Secrets-scanner pattern coverage.** The built-in patterns
  predate several common key formats, and some current ones don't match
  what they're named for: Anthropic keys (`sk-ant-api03-…`, the hyphens
  stop `sk-[A-Za-z0-9]{20,}` from matching), OpenAI project keys
  (`sk-proj-…`, same issue), GitHub `gho_`/`ghu_`/`ghs_`/`ghr_` tokens,
  Slack `xox[abpors]-`, Google `AIza…`, Stripe `sk_live_`/`rk_live_`, Hugging
  Face `hf_`, generic `Authorization: Bearer` headers pasted into prompts.
  Add each with a positive and negative test case; let operators add
  patterns via `middleware_config.secrets_scanner.extra_patterns` and
  disable built-ins by name. Given this gateway's own target clients, a
  leaked Anthropic key is one of the most likely real-world hits.
- [ ] **P4.13 — Fail closed on misconfiguration, not only on middleware
  errors.** The pipeline's fail-closed default protects against middleware
  *bugs*, but several config mistakes silently disable protection: an
  unknown YAML key (P0.14), an unhonoured config field (P0.15), an unknown
  `api_format` (P0.13), a `middleware_config` block for a middleware that
  isn't enabled. Consolidate these into one rule: **any config that cannot
  be honoured exactly as written is a startup/reload error**, with a
  `settings.strict_config: false` escape hatch for migrating from the
  Python config.
- [ ] **P4.14 — Refuse insecure listen configurations by default.** The
  defaults are `listen_host: 0.0.0.0` and no auth, which on a homelab
  machine exposes an open proxy to paid API keys on the LAN (or further,
  via UPnP/port forwards). At startup, if auth is open **and** the listen
  host is not a loopback address, refuse to start unless
  `settings.allow_unauthenticated_network_access: true` is set explicitly.
  Also change the shipped default `listen_host` to `127.0.0.1`; containers
  override it in their own manifests (P3.4).
- [x] **P4.15 — Upstream 4xx relayed with status but a normalized body;
  upstream auth failures become 502 (see P0.2).** The Python reference
  relays 4xx bodies verbatim (`main.py` passthrough; `proxy.py` wraps them in
  a 200 SSE event when streaming). Go instead relays the status with only a
  bounded message string, so proxy HTML pages and oversized bodies aren't
  reflected. Upstream 401/403/407 means the *gateway's* `api_key_env`
  credential was rejected, not the client's. Relaying 401 would make clients
  think their gateway key is wrong, and OpenAI's 401 message quotes a
  fragment of the rejected key. The client gets a 502 with the message
  "upstream rejected the gateway's credentials" (no upstream text), and the
  server log gets the upstream name and status. *Decision 2026-09-13 (user):
  keep the 502 and that explicit message. Verified by the "401 is the
  gateway's credential" case in `chat_test.go`.* Upstream messages echoing
  pseudonymized request text are not reverse-substituted (the client sees
  fake values); revisit with P0.4.
- [x] **P4.16 — Buffered streams are re-synthesized, not string-substituted
  (see P0.3, P0.4).** The Python reference's buffered mode runs the response
  pipeline over extracted content, discards the result, then sends the
  original bytes with fake → real string replacement. So redaction never
  reaches the client, and a real value containing `"` or `\` corrupts the
  JSON. Go collapses the stream per choice, runs the pipeline, and emits
  fresh OpenAI chunks. Trade-offs: buffered clients get one content chunk
  per choice (no token-level chunks, which buffering already made moot), and
  `logprobs` and non-string delta fields other than `tool_calls` are dropped.
  Upstream `error` events pass through verbatim. Undecodable data lines are
  dropped, because buffered mode exists so nothing unscanned reaches the
  client. Passthrough also decodes chunks rather than substituting bytes, but
  only when the request has pseudonyms to reverse.
  *Decision 2026-09-13 (user): keep one content chunk per choice. Replaying
  the original chunk boundaries would still arrive after the same wait, so
  it would only be cosmetic.*

---

## Explicit non-goals (carried over from the Python reference)

- No bundled TLS *certificate management* (ACME/renewal) — P4.4's optional
  TLS listener takes cert/key paths, it doesn't manage their lifecycle.
- `x-client-id` remains advisory-only, never a security boundary — the same
  auth tiers as the Python reference (per-user table, or shared key, or
  open/anonymous). P0.10/P4.11 follow directly from this: anything keyed on
  client identity for *security* purposes must use authenticated identity.
- No bundled local embedding model for the semantic cache — it calls a
  configured upstream's real `/embeddings` endpoint, same as the Python
  reference.
- No admin API or UI for editing configuration — config is a file,
  reloaded by P1.3. This keeps the write path for security settings out of
  the network-facing attack surface.
- No per-request prompt/response body logging, at any log level. Debugging
  aids that need bodies belong in tests against the P3.5 harness, not in
  production logs.
