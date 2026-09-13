# AiGateway (Go) — Build Plan

Status snapshot: 2026-09-12. This is a from-scratch Go rewrite of `../PyAiGateway`,
a self-hosted, provider-agnostic AI proxy gateway for home labs (DLP, cost
control, resilience for outbound AI traffic). The Python version is the
reference for behavior and config shape; this plan tracks where the Go port
already matches it, where it deliberately deviates, and what's still open.

Numbering is `P<phase>.<item>`. Mark an item done in place (`[x]`) rather than
deleting it, so this stays a record of what shipped and why, not just a todo
list — same rationale as the Python repo's `roadmap.md`.

---

## Phase 0 — Core skeleton (DONE)

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
- [x] **Upstream manager** (`internal/upstream`): circuit breaker
  (CLOSED/OPEN/HALF-OPEN with single-probe semantics), background-health-
  check gating, linear-backoff retry within an upstream, fallback across a
  route's ordered upstream list, and a deliberate non-streaming-vs-streaming
  distinction on *when* success is recorded (streaming records success on
  headers-received, before any body byte, since a partial stream can't be
  safely retried).
- [x] **Handlers**: `POST /v1/chat/completions` (streaming + non-streaming,
  buffered vs. passthrough stream modes — buffered is forced whenever
  `secrets_scanner` is active, matching the Python reference's P3.1 finding
  that flag-after-leak is unacceptable for credentials), `GET /v1/models`,
  `GET /health`. Request body size capped (`max_request_bytes`).
- [x] **Hot-reload primitive**: `server.Server` holds an `atomic.Pointer[AppState]`;
  swapping in a new `AppState` is a single atomic store, so no request ever
  observes a torn old-pipeline/new-upstream-config combination. The *watcher*
  that calls `Swap` on a config-file change is not wired yet (Phase 1).
- [x] Table-driven unit tests exist for: `contentpolicy`, `costtracker`,
  `piiredactor`, `pseudonymizer`, `ratelimiter`, `secretsscanner`.

**Known gaps in Phase 0 work** (not regressions, just not started):
`tokenratelimiter` has no test file yet; no tests exist yet for `chatmodel`,
`config`, `upstream`, `server`, or `handlers`. `web/` is an empty placeholder
directory. `go.mod` has exactly two dependencies (`chi`, `yaml.v3`) — keep the
dependency surface this small deliberately (see P4.10).

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
- [ ] **P1.2 — Gemini translator.** Not present in the Python reference at
  all — new work, not a port. Same `Translator`/`StreamTranslator` shape.
  Scope this after Anthropic; Gemini's function-calling and safety-rating
  shapes are different enough to warrant their own design pass rather than
  reusing the Anthropic translator's structure.
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
- [ ] **P1.4 — `/v1/embeddings` and `/v1/completions` passthrough.** Routed
  the same way as chat (auth, model routing, fallback, audit logging) but
  skip the chat-message DLP pipeline since these bodies aren't chat messages
  — matches the Python reference's documented behavior.
- [ ] **P1.5 — Audit persistence.** A `db` package backing an `audit_log`
  table, `token_usage`, `cost_tracking`, and `blocked_events` (`ts`,
  `client_id`, `model`, `middleware_name`, `reason`, `direction` — never the
  matched content itself). Batched writes off the request hot path.
  Auto-retention pruning on startup (`retention_days`, `-1` disables).
  **Driver decision:** use `modernc.org/sqlite` (pure Go, no cgo) rather than
  `mattn/go-sqlite3` — see P4.2, this matters more in Go than it did in
  Python because a static, cgo-free binary is the whole point of shipping Go
  here.
- [ ] **P1.6 — `/metrics` and `/metrics/prometheus`.** Auth-gated (learn from
  the Python reference's own P1.2 finding — it originally shipped these
  unauthenticated). Token counts, costs, cache stats, upstream health,
  recent audit tail, `blocked_events_by_middleware` + recent blocked events.
- [ ] **P1.7 — `/dashboard`.** Self-contained HTML (no CDN deps), unauthenticated
  route serving a static page that itself prompts for a gateway key
  client-side and polls the (auth-gated) `/metrics` endpoint. Port the
  Python reference's `gateway/static/dashboard.html` behavior rather than
  redesigning it.

---

## Phase 2 — Caching and horizontal scale

- [ ] **P2.1 — Exact-match response cache.** In-memory LRU, keyed on
  SHA-256(model + temperature + max_tokens + messages). Bypass via
  `X-Cache-Control: no-cache`. Never caches streaming requests or error
  responses. Stats at `/metrics`.
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
- [ ] **P2.3 — Redis-backed `Store` implementation.** Sorted-set sliding
  windows for the two rate limiters, a per-client hash+TTL for the
  pseudonymizer session map, a Redis-backed response cache. Config-gated
  (`redis.enabled`), zero-dependency in-memory path stays the default.
- [ ] **P2.4 — Semantic cache (optional).** Layered in front of the exact
  cache; embeds the prompt via the configured upstream's real
  `/embeddings` endpoint (no bundled local model), cosine-similarity scan
  scoped per-model. Off by default; keep `similarity_threshold` high in the
  shipped template (a near-miss match means a request can be answered from
  another client's cached response).

---

## Phase 3 — Test coverage, CI, deployment

- [ ] **P3.1 — Fill test gaps.** `chatmodel` (message scan/sanitize/transform
  helpers), `config` (validate + glob routing edge cases), `upstream`
  (circuit breaker state machine + retry/fallback against `httptest`
  fakes), `server/auth` (constant-time paths, all three auth tiers),
  `handlers` (end-to-end via `httptest`: blocked requests, streaming buffer
  vs. passthrough mode, timeout handling), `tokenratelimiter` (currently
  untested).
- [ ] **P3.2 — Concurrency correctness.** Run the full suite under
  `go test -race` in CI as a hard gate — this is Go-specific value the
  Python port couldn't get for free (GIL aside, asyncio has its own races);
  the atomic `AppState` swap and every shared middleware state map are
  exactly the kind of thing `-race` catches early.
- [ ] **P3.3 — CI pipeline.** Build, `go vet`, `golangci-lint`, `go test
  -race ./...`, `govulncheck` (see P4.10).
- [ ] **P3.4 — Deployment artifacts.** Multi-stage `Dockerfile` (static
  binary, distroless or scratch final stage — no cgo means this is
  genuinely tiny), `docker-compose.yml` (+ optional `redis` service, same
  shape as the Python reference's), k3s manifests. Port `HOMELAB_DEPLOYMENT.md`
  and `CLIENT_SETUP.md`, adjusted for a single static binary as a first-class
  deployment option (systemd unit) alongside containers — this wasn't
  really viable for the Python version and is a genuine differentiator here.

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
  friction Go was chosen to avoid.
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
- [ ] **P4.5 — Bound the DLP scan surface.** `secrets_scanner`,
  `pii_redactor`, `context_pseudonymizer`, and `content_policy` all run
  attacker-controlled input through regex and entropy scans. Cap the input
  length fed to each scan (already implicitly bounded by
  `max_request_bytes`, but re-check per-message rather than only per-body)
  and add a per-middleware processing deadline via `context.Context` so a
  pathological input can't stall a request indefinitely — regex ReDoS is a
  real concern for hand-written alternation patterns like the ones in
  `secretsscanner.go`.
- [ ] **P4.6 — Fuzz tests for the regex-heavy middleware.** `go test -fuzz`
  is native and cheap; run it against `secretsscanner`, `pseudonymizer`
  (detectors), and `content_policy`'s pattern matching. This is something
  the Python port didn't get for free and is worth doing specifically
  *because* P4.5 identifies these as the attacker-facing parsers.
- [ ] **P4.7 — Keep the "never log a secret" guardrail extensible.** Every
  new config field that can hold sensitive material (a future per-user
  resolved key, a future upstream credential cache, etc.) must use
  `secret.String`, not a bare `string`. Add a CI check (a small grep-based
  test or golangci-lint custom rule) that flags new struct fields named
  like `*key*`/`*secret*`/`*password*`/`*token*` that aren't typed
  `secret.String`, so this stays enforced rather than convention-only.
- [ ] **P4.8 — Never leak matched content in block reasons.** Already true
  in the current `secretsscanner`/`contentpolicy` code (`"<pattern> pattern
  matched"`, not the match itself) — carry this invariant forward
  explicitly into `/metrics`, `/dashboard`, and `blocked_events` (P1.5/P1.6):
  those surfaces must only ever show the generic reason string, matching
  the Python reference's P2.3 finding.
- [ ] **P4.9 — Metrics/dashboard auth from day one.** Build `/metrics` and
  `/dashboard` (P1.6/P1.7) auth-gated from the first commit that adds them,
  rather than shipping open and fixing it later as the Python reference's
  own history (P1.2) shows it did.
- [ ] **P4.10 — Keep the dependency surface small, and check it.** Currently
  two third-party modules (`chi`, `yaml.v3`). Prefer stdlib
  (`net/http`'s `ServeMux` pattern routing is sufficient for this route
  count as of Go 1.22+; keep `chi` only if a concrete need — e.g. its
  middleware chaining ergonomics — justifies it) over adding routers/
  frameworks. Any new dependency (`fsnotify`, a Redis client, a Prometheus
  client library) should be a deliberate, reasoned addition, and
  `govulncheck` (P3.3) should run in CI from the point any dependency beyond
  the current two is added.

---

## Explicit non-goals (carried over from the Python reference)

- No bundled TLS *certificate management* (ACME/renewal) — P4.4's optional
  TLS listener takes cert/key paths, it doesn't manage their lifecycle.
- `x-client-id` remains advisory-only, never a security boundary — the same
  auth tiers as the Python reference (per-user table, or shared key, or
  open/anonymous).
- No bundled local embedding model for the semantic cache — it calls a
  configured upstream's real `/embeddings` endpoint, same as the Python
  reference.
