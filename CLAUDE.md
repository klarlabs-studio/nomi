# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Nomi is a local-first, state-driven agent platform. A Go runtime (`nomid`) serves a REST + SSE API on `:8080`, and a Tauri desktop app (React 19 + shadcn/ui) is the primary client. Data is persisted in SQLite at the OS app-data dir (macOS: `~/Library/Application Support/Nomi/nomi.db`).

## Common commands

```bash
# Backend (Go)
make dev            # go run cmd/nomid/main.go — starts API on :8080
make build          # builds bin/nomid
make test           # go test -v -race -coverprofile=coverage.out ./...
go test ./internal/runtime/...                 # single package
go test -run TestRunStateMachine_Transition ./pkg/statekit   # single test
make lint           # golangci-lint run ./...
make fmt            # go fmt + prettier in app/

# Migrations (embedded in binary — runs automatically on nomid startup)
make migrate-up     # go run cmd/migrate/main.go up
go run cmd/migrate/main.go        # show current version/dirty status

# Desktop app (Tauri + Vite)
make app-dev        # cd app && npm run tauri dev   (dev server at :5173)
make app-build      # cd app && npm run tauri build
cd app && npm run lint
cd app && npm run test:e2e        # Playwright against vite preview on :4173

# Note: running via plain vite preview (outside Tauri) uses polling mode;
# native SSE live push is only available through the Tauri bridge.

# Deps
make deps           # go mod download + app npm install
```

E2E tests (`app/e2e/`) require `nomid` already running on `:8080` — start the backend in a separate terminal first. Playwright spins up its own `vite preview` server on `:4173`.

## Architecture

### Runtime flow (Run → Plan → Steps → Tools)

The execution model is built around two state machines in `pkg/statekit`:

- `RunStateMachine` — transitions a `Run` through `created → planning → plan_review → executing → (awaiting_approval ↔ executing) → completed|failed|cancelled`.
- `StepStateMachine` — `pending → ready → running → done|failed|blocked`, with retry loops.

`internal/runtime/engine.go` (`Runtime`) is the orchestrator that ties everything together. When a run executes a step whose tool capability is gated by the assistant's `PermissionPolicy`, the runtime pauses the run (`RunAwaitingApproval`), creates an approval via `permissions.Manager`, and resumes on approval resolve. The permission engine (`internal/permissions/engine.go`) evaluates `capability` strings (e.g. `filesystem.write`) against per-assistant rules with `allow|confirm|deny` modes and wildcard matching.

### Wiring (read `cmd/nomid/main.go` first)

Boot order and dependency graph:
1. `db.New` opens SQLite (WAL, foreign keys on), then `database.Migrate()` runs embedded migrations from `internal/storage/db/migrations/*.sql` via `golang-migrate` + `iofs`.
2. `events.EventBus` wraps an `EventRepository` — every domain event (`EventRunCreated`, step transitions, approvals, etc.) is persisted and fan-outed to filtered subscribers. SSE streaming at `/events/stream` is a subscriber.
3. `permissions.Engine` + `permissions.Manager` (backed by `ApprovalRepository`) manage approval workflow.
4. `tools.Registry` + `tools.Executor` — `RegisterCoreTools` registers `filesystem.read`, `filesystem.write`, `command.exec`, `filesystem.context`. Tools implement `Tool{ Name, Capability, Execute }`.
5. `memory.Manager` over `MemoryRepository` — memory is scoped (default `workspace`) and keyed to assistants.
6. `runtime.NewRuntime` receives all of the above.
7. `connectors.Registry` — pluggable external transports (Telegram is the only concrete one). Config is loaded from DB (`connector_configs` table), not env. New connectors implement `connectors.Connector` and are registered at boot.
8. `api.Router(...)` assembles Gin routes; `/health`, `/runs`, `/assistants`, `/approvals`, `/events`, `/memory`, `/tools`, `/connectors`, `/provider-profiles`, `/settings`. CORS is permissive for Tauri.

The port is read from the `app_settings` table via `AppSettingsRepository.GetOrDefault("api_port", "8080")` — not from env vars or flags.

### Persistence layout

SQLite repositories live in `internal/storage/db/*.go` (one file per aggregate group). Migrations are **embedded** via `//go:embed migrations/*.sql` — always add new migrations as both `NNNNNN_name.up.sql` and `NNNNNN_name.down.sql`, sequential numbers. The runtime binary carries migrations; no external tool is required at deploy.

### Desktop app

`app/` is a Tauri v2 shell around a Vite + React 19 + TypeScript frontend with shadcn/ui primitives in `app/src/components/ui/`. The React app talks to `nomid` at `http://127.0.0.1:8080` (hardcoded in `app/src/lib/api.ts`). The Rust side (`app/src-tauri/src/main.rs`) proxies the `/events/stream` SSE into Tauri events (`start_event_stream` command) so the React UI subscribes via `useTauriEvents` instead of opening `EventSource` directly.

Tabs/features in `App.tsx`: Chats, Assistants, Approvals, Memory, Events, Settings (Connections / AI Providers / Plugins).

## Conventions

- Module path: `github.com/felixgeelhaar/nomi`. Internal packages are genuinely internal — don't import them from `app/` or `pkg/`.
- Domain types and status enums live in `internal/domain/models.go`; keep state strings in sync with the machines in `pkg/statekit/run_step_sm.go` when adding statuses.
- New tools go through `tools.Registry` + a capability string matching a permission rule — never bypass the permission engine.
- New connectors implement `connectors.Connector` and register in `cmd/nomid/main.go`; config reads go through `ConnectorConfigRepository`, not env.
- Events must be published via `eventBus.Publish(...)` so they get both persisted and streamed.

## Roadmap (roady)

Planning and task state live in `.roady/` — this is the source of truth for what's shipped and what's next. Read it before starting a feature.

- `.roady/spec.yaml` — product spec: feature list with descriptions (includes V1 scope *and* post-V1 items). `.roady/spec.lock.json` is the locked hash.
- `.roady/plan.json` — decomposed tasks (234 total at 2026-05-09) per feature with `depends_on` graph.
- `.roady/state.json` — per-task status. Backlog is **234/234 done** as of 2026-05-09 cycle 2; new features land via `roady_add_feature` + `roady_generate_plan` rather than editing JSON.
- `.roady/events.jsonl` — hash-chained audit log of task transitions and plan generations.
- `.roady/policy.yaml` — `max_wip: 3`, `allow_ai: true`.

**Shipped (V1 + post-V1 cycle 2):** core runtime + state machines, permission/approval engine, core tools (fs read/write/patch, command.exec, folder context), Mnemos memory + case-insensitive search, SQLite storage + embedded migrations + hash-chained audit (`/audit/verify`), REST API (Gin), SSE events, **Tauri Native IPC** (Rust `reqwest` + `window.emit()`) as the only real-time event path, Tauri desktop UI with all tabs (deep-linked approval surface), Telegram connector + plugin architecture, LLM provider profiles + per-assistant model override, folder context attachment end-to-end, collaborative plan review (`/runs/:id/plan/approve` + `/runs/:id/plan/edit` + `/runs/:id/replan`), planner with multi-step LLM plans + JSON mode + few-shot exemplars + self-repair retry + replan-on-failure (bounded by `MaxReplansPerRun`), prompt-injection trust-boundary tags, planner-context budget envelope, **filesystem.patch** with `git apply --check` dry-run + 3-way fallback + path pre-flight, **coding-agent** flagship recipe (`examples/coding-agent/`), Prometheus `/metrics` (per-provider planner + edit-distance + step counters), planner golden corpus + adversarial fixtures + threshold gate (`make eval-live`), deterministic fake-LLM e2e fixture (Playwright globalSetup), unified approval surface with WCAG-AA semantic tokens + polite ARIA, chat-list run search + branch-from-here, diff preview with per-hunk skip + side-by-side toggle.

**Shipped (post v0.2.1):** sandboxed executor backends (local/docker/gvisor), Recipe registry + signed YAML bundles + 9 built-in recipes, scheduled runs + NL→cron via LLM, skill induction (Jaccard + cosine embedding clustering + LLM synthesis), auto-learning preferences loop, embeddings provider integration, WhatsApp/Slack/Discord plugins, plugin context-source consumption in planner, FTS5 memory search (`memory_fts` + bm25 ranking), OS notifications on `approval.requested` (`tauri-plugin-notification` + Web API fallback), **macOS menu bar / system tray** with live Approvals(N) badge + pause-all + new-chat + settings + quit + 3 state icons (idle/active/awaiting).

**Pending / deferred:**
- **Shiki syntax highlighting in DiffPreview** — class names already match what a future Shiki swap will reuse; the worker dep was deferred when per-hunk skip + side-by-side shipped.
- **NOMI_EVAL_LIVE provider matrix** — `make eval-live` runs the fake-LLM corpus + adversarial fixtures with the threshold gate; live-against-real-providers pass-rate reporting is the post-deferral.
- **Scout browser plugin** — Scout already exists as a standalone tool; first-party plugin integration is the next big capability.
- **eBPF / DNS egress allowlist** — `network.egress` capability today flips `--network=none` vs `--network=bridge`; per-domain allowlist enforcement needs a userland firewall layer.

**Workflow when picking up work:** check `.roady/state.json` for the task, run `roady` MCP tools (`roady_get_ready_tasks`, `roady_transition_task`) to claim it, keep WIP ≤ 3. Spec/plan changes go through `roady_review_spec` / `roady_generate_plan` rather than editing YAML by hand — the `events.jsonl` chain breaks otherwise.
