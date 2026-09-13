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

- **Last updated:** 2026-09-12
- **State:** On branch `m1/trustworthy-core` (uncommitted as of this note):
  `gofmt -w .` + `go mod tidy` done; P3.5, P0.1, P0.2 done. Wiring moved
  from `cmd/aigateway` into `internal/app` so tests serve the real router;
  `config.Parse` added for inline-YAML configs. `go test ./...` passes;
  `-race` not run (no cgo on this machine).
- **Next up:** P0.3 → P0.4 (both reshape the stream handlers; do together),
  then P0.5 onward.
- **In progress:** none.
- **Open questions for the user:** commit split (suggest: formatting/tidy
  commit, then harness + P0.1 + P0.2). P4.15 (upstream 401/403 → 502) is a
  judgement call worth a glance.
- **Test note:** keep `retry_attempts: 0` in harness configs until P0.6;
  the hardcoded backoff still sleeps 1s after the final attempt.

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
- [ ] **P0.3 — Buffered streaming discards the response pipeline's rewritten
  text.** `serveBufferedStream` calls `pipe.RunResponse(ctx, content, gctx)`
  and ignores the returned text, then writes the *original* raw SSE bytes
  (`chat.go:171-184`). Response-phase redaction by `pii_redactor` (and any
  other rewriting middleware) is silently not applied to streamed responses,
  even in the mode that exists specifically to make response DLP
  enforceable. **Fix:** after a non-blocking `RunResponse`, re-emit the
  stream as synthesized OpenAI chunks from the rewritten content (preserving
  `id`/`model`/`finish_reason`/`usage`/tool-call deltas), rather than
  string-substituting into raw bytes. This also resolves P0.4 for buffered
  mode.
- [ ] **P0.4 — Reverse pseudonymization is applied to raw JSON wire bytes.**
  Both stream modes call `gctx.ReverseSubstitute` on raw SSE lines
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
- [ ] **P0.5 — Background health checks don't exist.** Implement
  `internal/upstream/health.go`: one goroutine per `Manager` generation,
  ticking at `health_check.interval_seconds`, probing each upstream's
  `health_path` (skip upstreams without one) with `timeout_seconds`, calling
  `RecordCheck`. The goroutine's lifetime must be tied to the `AppState`
  generation (a `context.CancelFunc` stored on `Manager`, cancelled on swap
  by P1.3) — otherwise each hot reload leaks a checker. Until this ships,
  `Validate` should warn that `health_check.enabled` has no effect.
- [ ] **P0.6 — Retry behaviour ignores config and wastes time.**
  `Manager.backoff` hardcodes `(attempt+1) × 1s` (`manager.go:292`) instead
  of using `retry_delay_seconds`, and it sleeps after the *final* attempt
  before moving to the next fallback upstream. Also: `RetryAttempts` is not
  validated as `>= 0`, and a negative `FailureThreshold`/`0` opens the
  circuit on the first failure. Fix all three; add jitter (±20%) so multiple
  clients don't retry in lockstep.
- [ ] **P0.7 — Streaming error events are built by string concatenation.**
  `chat.go:138` and `chat.go:179` splice `err.Error()` / `gctx.BlockReason`
  into a JSON literal. Any `"` in an error (URL-bearing net errors, upstream
  bodies) produces malformed JSON the client can't parse. **Fix:** a single
  `writeSSEError(w, status, message)` helper using `json.Marshal`.
- [ ] **P0.8 — Internal details leak to clients in error messages.** Non-
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
- [ ] **P0.9 — Unbounded memory on upstream responses.** Buffered stream mode
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
- [ ] **P0.10 — Pseudonymizer sessions leak real values across clients.**
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
- [ ] **P0.11 — Pseudonymizer session updates race (lost update / fake
  collision).** `getOrCreate` returns *copies*, the request extends them
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
- [ ] **P0.12 — Auth table accepts unsafe configurations.** `Validate` does
  not reject: a user with an empty `gateway_key` (an empty presented
  credential then authenticates as that user, `auth.go:50`); two users
  sharing a key (identity then depends on random map iteration order);
  `users[].upstream` naming an undeclared upstream (silently skipped in
  `Send`, surfacing as a confusing "all upstreams unavailable"); or
  `auth_key` still set to the template's `changeme`. Reject all four.
  Separately: `subtle.ConstantTimeCompare` returns early on length mismatch,
  leaking key length — compare `sha256(presented)` against pre-computed
  `sha256(key)` digests instead (fixed-length, still constant-time).
- [ ] **P0.13 — Streaming never goes through the provider translator.**
  `SendStream` translates the *request* but returns the upstream body raw,
  and `serveStream` assumes it is OpenAI-shaped SSE. Harmless while only
  `openai` exists, but P1.1 cannot work without it. Wire
  `Translator.NewStreamTranslator()` into the stream path now (identity for
  openai) so P1.1 is purely additive. Also make `translatorFor` return an
  error for an unknown `api_format` (`manager.go:131` currently falls back
  to openai silently) and reject unknown `api_format`/`auth_type` values in
  `Validate`.
- [ ] **P0.14 — Smaller correctness items (batch into one change).**
  - `SourceIP` splits `RemoteAddr` on the last `:` (`auth.go:100`), which
    leaves brackets on IPv6 (`[::1]`); use `net.SplitHostPort`. With
    `trust_proxy_headers`, taking the **leftmost** `X-Forwarded-For` entry
    trusts a client-supplied value even behind a proxy (proxies append);
    replace the boolean with `trusted_proxies: [CIDR...]` and walk XFF
    right-to-left, stopping at the first untrusted hop.
  - `http.Server` has no `ReadHeaderTimeout`, `ReadTimeout`, or `IdleTimeout`
    (`main.go:75`) — slowloris-style connection exhaustion. Set
    `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`, and leave `WriteTimeout`
    unset (streams) in favour of P0.9's `stream_timeout`.
  - `token_rate_limiter` never evicts idle keys (unlike `rate_limiter`), so
    IP-keyed anonymous clients grow its map forever. Reuse the same cleanup.
  - Blocks always return **400** (`chat.go:63`). Rate-limit and budget blocks
    should return **429** with `Retry-After`; content/DLP blocks stay 400.
    Carry the status on `GatewayContext` alongside `BlockReason`.
  - `audit_log` runs in the request phase, so it logs `candidates[0]` rather
    than the upstream that actually served the request, and never records
    status, latency, tokens, or block outcome. Split into a request-phase
    capture and a post-response write (feeds P1.5). It also opens the log
    file on every request and creates it world-readable (`0o644`) despite
    containing source IPs — keep the handle open, use `0o640`.
  - The half-open probe slot is released *before* the failure is recorded in
    `Send` (`manager.go:170-175`), letting a second probe slip through.
    Release after recording. *Code reordered 2026-09-12 during P0.2 (both
    `Send` and `SendStream` now record, then release); the concurrency test
    is still owed.*
  - `ReverseSubstitute` and `buildSubstituter` re-sort keys and do
    O(keys × text) `strings.ReplaceAll` passes per call — per SSE line in
    passthrough mode. Build a `strings.Replacer` once per request.
  - `/health` is unauthenticated and returns upstream names plus circuit
    state. Keep liveness open, but move per-upstream detail behind auth
    (`/health` → `{"status":"ok"}`; detail moves to `/metrics`, P1.6).
  - `config.Load` doesn't reject unknown YAML keys, so a typo like
    `midleware:` silently disables all DLP. Use `yaml.Decoder.KnownFields(true)`
    at the top level, and reject `middleware_config` entries for middleware
    that isn't in `middleware:` (or at least warn).
  - The schema doc comment promises `auth_key` can be overridden via env, but
    `Load` doesn't implement it. Add `AIGATEWAY_AUTH_KEY` (and a
    `gateway_key_env` alternative per user) so the shared secret doesn't have
    to live in a file that tends to get committed.
- [ ] **P0.15 — Config fields accepted but not honoured.** Until the owning
  item ships, each of these should produce a startup warning (not silent
  acceptance), so an operator isn't misled into thinking a protection is
  active:

  | Field | Default | Honoured by |
  |-------|---------|-------------|
  | `cache.enabled` | `true` | P2.1 |
  | `cache.semantic.*` | off | P2.4 |
  | `redis.*` | off | P2.3 |
  | `settings.audit_db`, `settings.retention_days` | set | P1.5 |
  | `resilience.health_check.*` | enabled | P0.5 |
  | `resilience.retry_delay_seconds` | 1.0 | P0.6 |
  | `upstreams.*.api_format: anthropic\|gemini` | — | P1.1, P1.2 (reject until then) |

  Consider flipping `cache.enabled` to default `false` when P2.1 lands:
  response caching across clients is a data-sharing decision the operator
  should make explicitly.

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
  against PyAiGateway.
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
  fragment of the rejected key. The client gets a generic 502 and the server
  log gets the upstream name and status. Upstream messages echoing
  pseudonymized request text are not reverse-substituted (the client sees
  fake values); revisit with P0.4.

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
