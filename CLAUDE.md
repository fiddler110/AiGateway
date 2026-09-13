# AiGateway (Go)

Self-hosted AI proxy gateway for home labs: OpenAI-compatible ingress, DLP middleware
(secrets scanning, PII redaction, reversible pseudonymization), cost/rate limits, and
upstream resilience (circuit breaker, retry, fallback). A from-scratch Go rewrite of
`D:\Development\PyAiGateway`. The Python version is the behavioral reference, not a spec:
deviate where Go allows a better or safer design, and record why in PLAN.md Phase 4.

## Start of every session

1. Read `PLAN.md`. Start with **Current focus** at the top, then find the item you're
   working on. It's the source of truth for what's done, open, and in progress.
2. Check that the plan still matches the code before acting on it. File/line references
   in PLAN.md are from 2026-09-12 and will drift, and code sometimes lands before the
   plan is updated.
3. Unless the user says otherwise, work in milestone order (PLAN.md "Sequencing").
   Phase 0b (P0.x) comes first: those are live bugs in the request path.

## Commands

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .                      # must print nothing before committing
go run ./cmd/aigateway -config config.yaml
```

- Go 1.23.2 on Windows. `go test -race` needs cgo, so it won't run on this machine
  without a C toolchain. Use WSL or rely on CI, and say so instead of skipping it silently.
- `config.yaml` is gitignored and doesn't exist by default. Copy
  `config/aigateway.blank.yaml` to create it. The blank config makes every chat request
  return 400 on purpose.
- Upstream API keys come from environment variables named in config (`api_key_env`).
  Never put them in config files.

## Layout

| Path | Role |
|------|------|
| `cmd/aigateway/main.go` | Wiring: config → `AppState` → router → server |
| `internal/config` | YAML schema, defaults, validation, glob model routing |
| `internal/server` | `AppState` atomic swap (hot reload), auth, body limits, error envelope |
| `internal/handlers` | `/v1/chat/completions` (stream + non-stream), `/v1/models`, `/health` |
| `internal/pipeline` | Middleware interface, fail-open/closed chain, `GatewayContext`/`Scratch` |
| `internal/middleware/*` | The nine middleware; `registry.go` maps config names to constructors |
| `internal/upstream` | Forwarding, circuit breaker, retry/fallback |
| `internal/provider` | `Translator` boundary; only `openai` (identity) exists so far |
| `internal/chatmodel` | Canonical OpenAI request types; unknown JSON fields kept in `Extra` |
| `internal/secret` | `secret.String`, which redacts itself in logs and `fmt` |

## Invariants (don't break these)

- **Never log or return secrets or matched content.** Sensitive fields use `secret.String`.
  Block reasons are generic (`"<middleware>: ... blocked"`), never the matched text.
  Never log request or response bodies at any level.
- **Middleware and handlers only see the canonical OpenAI shape.** Provider translation
  happens only at the upstream boundary (`internal/provider`).
- **Handlers call `srv.CurrentState()` exactly once per request** and use that snapshot
  for the rest of the request.
- **DLP and enforcement middleware fail closed; accounting middleware fails open**
  (`failOpenDefaults` in `registry.go`).
- **Response middleware runs in the same order as request middleware, not reversed.**
- **Constant-time auth compare across all users with no early exit.** Don't replace it
  with a map lookup.
- **`x-client-id` is advisory.** Never key a security decision on it (see P0.10).
- **Streaming is forced into buffered mode whenever `secrets_scanner` is enabled.**
- **Keep the dependency surface small and cgo-free** (P4.2, P4.10). Ask before adding a
  module, and record the decision in P4.10.
- Only use stdlib `regexp` (RE2). Never add a backtracking regex engine.

## Working conventions

- **Tracking plan items:** tick an item `[x]` in place with a short "verified by: <test
  or check>" note. Don't delete or renumber items. Mark in-progress work `[~]` and note
  its branch.
- **Keep Current focus current:** before ending a session with unfinished work, update
  **Current focus** in PLAN.md with what's done, what's next, and any open question.
  That section is the handoff to the next session.
- **Tests:** every bug fix gets a regression test that fails without the fix. Prefer
  table-driven tests and `httptest` fake upstreams (see P3.5).
- **New config fields:** add them to `config/aigateway.blank.yaml` with a comment and
  validate them in `config.Validate`.
- **Git:** `main` is the default branch. Branch for work and commit only when asked.
- **Checking the reference:** compare behavior against the Python repo's `gateway/`,
  `tests/`, and `roadmap.md` (its shipped-fix notes explain why things are the way they
  are). Its `.aegis/security/threat-model` files are empty templates. Don't cite them.

## Known traps (as of 2026-09-12; remove each as it's fixed)

- Several config fields are accepted but do nothing yet: `cache`, `redis`, `audit_db`,
  `health_check`, `retry_delay_seconds` (P0.15). Don't assume a config key means the
  feature exists.
- `StreamTranslator` is defined but never called. Streams are forwarded raw (P0.13).
- `web/` is an empty placeholder for the P1.7 dashboard.
