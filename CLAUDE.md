# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Sub2API is an AI API gateway that distributes API quota from upstream AI product subscriptions (Anthropic/Claude, OpenAI/Codex, Gemini, Antigravity). Users authenticate with platform-issued API Keys; the gateway authenticates, selects an upstream account, applies billing/quota/concurrency/rate limits, and forwards the request to the upstream provider. Go backend (Ent ORM + Gin) + Vue 3 frontend (pnpm). Module path is `github.com/Wei-Shaw/sub2api` (upstream); this checkout is the fork `langgexyz/sub2api`.

## Repo topology

- `backend/` — Go service. Has its own `go.mod`, `Makefile`, `.golangci.yml`. **All `go` commands run from `backend/`**, not repo root.
- `frontend/` — Vue 3 SPA, managed with **pnpm** (never npm — see lockfile rule below). Built output is embedded into the Go binary.
- `datamanagement/` — separate Go module (`datamanagementd`), a host-side data-management process.
- `migrations/` lives under `backend/migrations/`; `backend/ent/` holds generated ORM code.
- Root `Makefile` orchestrates both halves; `backend/Makefile` is the source of truth for Go targets.

## Build & test commands

Root-level (orchestrates both):
```bash
make build                  # build-backend + build-frontend
make test                   # backend tests + frontend lint/typecheck/critical vitest
make secret-scan            # python3 tools/secret_scan.py
```

Backend (run from `backend/`):
```bash
make build                  # CGO_ENABLED=0 build to bin/server, version from cmd/server/VERSION
go run ./cmd/server/        # run locally
go test -tags=unit ./...        # unit tests
go test -tags=integration ./... # integration tests (need Postgres + Redis)
go test -tags=e2e ./internal/integration/...  # e2e (or make test-e2e-local)
golangci-lint run ./...     # lint (golangci-lint v2.7 pinned by CI)
go generate ./ent           # REQUIRED after editing ent/schema/*.go — regenerate + commit ent/
```

Run a single test: `go test -tags=unit -run TestName ./internal/service/`. Build tags are mandatory — a test file starts with `//go:build unit|integration|e2e`; without the matching `-tags` it is invisible to `go test`.

Frontend (from `frontend/`, or `pnpm --dir frontend ...` from root):
```bash
pnpm install                # MUST use pnpm; commit pnpm-lock.yaml alongside package.json
pnpm dev                    # vite dev server
pnpm build                  # vue-tsc -b && vite build
pnpm lint:check             # eslint, no autofix
pnpm typecheck              # vue-tsc --noEmit
pnpm exec vitest run <spec> # run a single test file
```
`make test-frontend-critical` runs a hand-picked allowlist of vitest specs (auth callbacks, payment, profile, settings) — keep these green; they gate CI.

## Hard rules (these break CI or production if violated)

- **Go 1.25.7 / 1.26 toolchain** as pinned by `backend/go.mod` and CI. Don't bump casually.
- **pnpm only.** Any `package.json` change requires a synced `pnpm-lock.yaml` commit, or CI `pnpm install --frozen-lockfile` fails. Don't mix npm-created `node_modules`.
- **Regenerate Ent after schema edits.** Edit `backend/ent/schema/*.go` → `go generate ./ent` → commit the generated `backend/ent/` files. Skipping this means your change silently does nothing.
- **Stub all new interface methods in tests.** Adding a method to a service/repository interface requires updating every test stub/mock or compilation breaks.
- PR checklist before pushing: unit + integration green, `golangci-lint` clean, lockfile synced, ent regenerated, interface stubs updated.

## Backend architecture

Layered, wired with **google/wire** (compile-time DI). `cmd/server/wire.go` (`//go:build wireinject`) declares providers; `wire_gen.go` is generated — regenerate via `go generate ./cmd/server`, don't hand-edit. `main.go` boots logging → config → `initializeApplication()` → HTTP server with graceful shutdown.

Request layers, outer to inner:
- `internal/server/` — `router.go` (`SetupRouter`/`registerRoutes`) wires middleware + route groups; `routes/` registers each module (`auth`, `user`, `admin`, `gateway`, `payment`); `middleware/` holds auth (JWT / admin / API-key), CORS, CSP/security headers, body limits, request logging.
- `internal/handler/` — HTTP handlers. The gateway proxy is the heart: `gateway_handler.go` plus `gateway_handler_chat_completions.go`, `gateway_handler_responses.go`, `failover_loop.go`. `handler/admin/` and `handler/dto/` split out admin endpoints and request/response DTOs.
- `internal/service/` — business logic (the bulk of the code). Large per-platform gateway services (e.g. `antigravity_gateway_service.go`), `account*.go` (account model, scheduling, model-mapping, usage), `admin_service.go`, billing/quota/pricing. `openai_ws_v2/` is the OpenAI Responses WebSocket path; `prompts/` holds prompt assets.
- `internal/repository/` — data access over Ent. Notable: `aes_encryptor.go` (credentials are AES-encrypted at rest), `*_cache.go` (Redis-backed caches for api-key/billing/quota), backup dumpers.
- `internal/domain/`, `internal/model/` — domain types and constants.
- `internal/pkg/` — reusable provider/protocol toolkits: `claude/`, `openai/`, `openai_compat/`, `gemini/`, `geminicli/`, `antigravity/`, `googleapi/` (request/response/stream transformers, OAuth, type schemas), plus `httpclient/`, `tlsfingerprint/`, `proxyurl/`, `errors/`, `ctxkey/`.
- `internal/setup/` — first-run setup wizard (web + `--setup` CLI); `internal/web/` embeds and serves the built frontend with runtime settings injection.

### Gateway endpoints (multi-protocol surface)

The proxy exposes several upstream-compatible API shapes under the API-key-authed `/v1`, `/v1beta`, and `/antigravity/*` groups (`routes/gateway.go`):
- Anthropic-compatible: `POST /v1/messages`, `/v1/messages/count_tokens`
- OpenAI-compatible: `POST /v1/chat/completions`, `/v1/responses` (+ `*subpath` and a WebSocket `GET /v1/responses`), `/v1/embeddings`, `/v1/images/generations`, `/v1/images/edits`; also bare `/responses`, `/chat/completions`, and `/backend-api/codex/responses` for Codex CLI.
- Gemini-compatible: `GET/POST /v1beta/models...`
- Antigravity: mirror of the above under `/antigravity/v1` and `/antigravity/v1beta`.

`RequireGroupAssignment` middleware rejects API keys not assigned to a group, with protocol-specific error bodies (`AnthropicErrorWriter` vs `GoogleErrorWriter`).

### Account selection & scheduling

`internal/service/account.go` defines the `Account` model and scheduling predicates (`IsSchedulable`, `IsRateLimited`, `IsOverloaded`, load factor, temp-unschedulable rules). **Model mapping** is central: each account maps requested model names to upstream names (`GetModelMapping`/`ResolveMappedModel`); bulk-editing accounts across different platforms can clobber these mappings and silently take an account offline — verify mappings after bulk edits. Sticky-session routing (session_hash → account_id), response-id stickiness, and scheduler score weights are configured under the gateway config (`StickySessionTTLSeconds`, `SchedulerScoreWeights`, etc.).

### Run modes

`config.RunMode` is `standard` (default) or `simple`. **Simple mode disables billing and quota checks** (`main.go` logs a warning) — useful for self-host/dev, not for metered production.

## Config & data

- Config via `config.yaml` + env (Viper); `SERVER_HOST`/`SERVER_PORT` override address. First run triggers the setup wizard unless auto-setup-from-env is enabled (Docker path).
- Postgres + Redis required for integration/e2e and runtime. Local dev defaults (per `DEV_GUIDE.md`): pg `sub2api/sub2api/sub2api` on 5432, Redis on 6379 no password.
- SQL migrations in `backend/migrations/NNN_*.sql` run in numeric order; some share a number with a/b suffixes.
- Built-in payment (EasyPay/Alipay/WeChat/Stripe) lives in `internal/payment/` + `handler` payment routes; see `docs/PAYMENT.md`.

## Further reading

- `DEV_GUIDE.md` — local environment setup, CI workflow details, and a running list of gotchas (lockfile sync, ent regen, model-mapping pitfalls).
- `docs/PAYMENT.md`, `docs/ADMIN_PAYMENT_INTEGRATION_API.md` — payment integration.
- Upstream: `github.com/Wei-Shaw/sub2api`. This fork: `github.com/langgexyz/sub2api`.
