# Changelog

All notable changes to Nomi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [Unreleased] - 2026-05-22 (Scheduled runs)

First cut of cron-driven scheduled runs (roady #124). A Schedule fires
a Run on a recurring cadence against a chosen assistant with a fixed
prompt. Background ticker polls every 30s and triggers via the runtime
exactly as a connector source would — schedule fires show up in the
audit log with `source = "schedule"`.

### Added
- `schedules` table (migration #27) with `assistant_id` FK + index on
  `next_fire_at` (partial, enabled-only).
- `domain.Schedule` + `db.ScheduleRepository` CRUD + `DueBefore`.
- `internal/scheduler` package — background ticker, cron parsing
  (robfig/cron/v3), fire dispatch through the existing
  `runtime.CreateRunFromSource` path.
- REST endpoints: `POST/GET/PATCH/DELETE /schedules`, `GET /schedules/:id`.
- Boot wire in `cmd/nomid/main.go` starts the scheduler with a 30s
  tick interval.

### Behavior notes
- Missed-fire policy: skip. A daemon down for 8 hours doesn't unleash
  8 catch-up runs on restart — it fires once on the next tick and
  advances `next_fire_at` to the next future cron slot.
- Invalid cron expressions disable the schedule and record the cause in
  `last_error`, rather than busy-looping.
- Schedules respect the assistant's permission policy — they go
  through the same approval flow a manual run would.

### Deferred
- Natural-language cron translation (e.g. "every weekday at 8am") —
  needs an LLM call + grammar guard; ship in a follow-up.
- Tauri Settings UI surface for schedule CRUD.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR5: assistant editor UI)

Exposes the executor backend choice in the desktop app's assistant editor.
A new Sandbox section sits between Memory and Permissions with a backend
dropdown and (for container backends) an image input. The dropdown is
populated from the live runtime — only backends nomid successfully
probed at startup appear, so a machine without Docker doesn't see
"docker" as an option.

### Added
- `GET /runtime/executor-backends` — returns the list of registered
  backend names (`local` always, plus `docker` and/or `gvisor` when
  available).
- `runtimeApi.executorBackends()` in the frontend API client.
- `Assistant.executor_backend` + `Assistant.sandbox_image` fields on
  the TypeScript API types and the `CreateAssistantRequest` shape.
- Sandbox section in the assistant editor: backend dropdown,
  contextual help describing each backend's isolation properties,
  conditional image input (hidden for the local backend), and a hint
  pointing users at the `network.egress` permission rule.

### Changed
- `internal/api/assistants.go` — request/response shape carries
  `executor_backend` + `sandbox_image`; UpdateAssistant persists both.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR4: network.egress + Prometheus)

Wires container-backend network egress through the existing permission
engine and emits per-backend Prometheus counters and duration
histograms. Deny-by-default holds: a container backend without an
explicit Allow on `network.egress` runs with `--network=none`. An
Allow rule promotes the container to `--network=bridge` (full
outbound). Domain allowlist enforcement at the bridge is reserved for
a follow-up — needs a userland firewall (eBPF or DNS allowlist) and
isn't shippable through `docker run` flags alone.

### Added
- `executor.NetworkMode` (`none` | `bridge`) on `Request`. Local
  backend ignores; container backends apply via `--network=<mode>`.
- Runtime evaluates the assistant's policy for `network.egress` at
  step time and injects the mode through `__network_mode`. Allow →
  bridge; everything else (Confirm/Deny/absent) → none.
- `nomi_executor_runs_total{backend,outcome}` counter — outcomes:
  `success` | `exit_nonzero` | `oom` | `timeout` | `error`.
- `nomi_executor_duration_seconds{backend}` histogram with buckets
  spanning sub-second (local exec) through 60s (container coldstart).
- `nomi_executor_oom_total{backend}` counter.
- `runtime.instrumentedBackend` decorator wraps every registered
  backend so metric emission is uniform and the executor package
  doesn't take a metrics dependency.

### Security
- `network.egress` is the first capability gated at the backend-
  config layer rather than tool-call layer. Confirm mode is treated
  as deny here since there's no approval UX for "should this run be
  allowed network access?" outside the existing tool-approval flow.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR3: gVisor)

Adds the gVisor (runsc) execution backend. Reuses the Docker code path
with `--runtime=runsc` so the container traps syscalls through a
user-space kernel instead of the host kernel — a meaningful upgrade
against kernel-exploit escapes for assistants running untrusted code.

### Added
- `executor.GvisorBackend` — thin composition over `DockerBackend` with
  `Runtime = "runsc"` pinned.
- `executor.DockerBackend.Runtime` field — supports any installed docker
  runtime (runsc, kata, custom) as a future extension point.
- `cmd/nomid/main.go` boot probe for the runsc runtime via `docker info`;
  backend registered only when reachable.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR2: docker)

Adds the Docker execution backend. Containers are rootless on the host
side (--user is image-default for now), pinned to --network=none,
--memory/--memory-swap equal so OOMKilled fires deterministically,
--cpus and --pids-limit bounded, --init reaping zombies, and
--rm so containers don't accumulate. Workspace bind-mounts at /workspace
and the container working directory translates host paths into the
mount. Exit code 137 is heuristically classified as OOM under the pinned
memory + swap config.

### Added
- `executor.DockerBackend` implementing `Backend` via the `docker` CLI
  (no SDK dependency).
- `executor.DockerBackend.Available(ctx)` boot probe — checks `docker`
  on PATH and `docker info` reachability.
- `executor.Request.WorkspaceRoot` + `executor.Request.Image` — backend-
  specific fields that the local backend ignores and container backends
  consume.
- Migration `000026_assistant_sandbox_image` — adds `sandbox_image TEXT
  NOT NULL DEFAULT ''` to `assistants`.
- `AssistantDefinition.SandboxImage` field + repo CRUD.
- `cmd/nomid/main.go` probes Docker availability at boot with a 3s
  timeout and registers the backend when present. Absence is normal
  (logged at debug level only when present).
- Live integration test `TestDockerLiveEcho` runs against the real
  daemon when available; skipped automatically without docker.

### Changed
- `tools/command.go` now forwards `workspace_root` and the runtime-
  injected `__sandbox_image` to the backend via `executor.Request`.
- `execution.go` injects `__sandbox_image` alongside `__sandbox` when
  the assistant has a SandboxImage configured.

### Roadmap
- PR3: gVisor (runsc) backend — reuses the docker code path with
  `--runtime=runsc`.
- PR4: `network.egress` capability, restricted bridge with domain
  allowlist, Prometheus counters per backend.
- PR5: Tauri Settings UI dropdown + image input + "Test sandbox"
  probe button.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR1: interface + local)

Introduces a pluggable execution-backend interface so future
container-isolated runtimes (Docker, gVisor) can replace the host-exec
path without touching the tool layer. PR1 ships the interface and
extracts current host-exec behavior into a `local` backend; zero
behavior change.

### Added
- `internal/runtime/executor` package — `Backend` interface,
  `Request`/`Result` types, `Registry` keyed by backend name,
  `LocalBackend` preserving the prior `os/exec.CommandContext` +
  `Setsid`/new-process-group behavior. Backend selection per-assistant
  via the new `AssistantDefinition.ExecutorBackend` field (default
  `"local"`).
- Migration `000025_assistant_executor_backend` — adds
  `executor_backend TEXT NOT NULL DEFAULT 'local'` to `assistants`.
- `Runtime.RegisterExecutorBackend` + `Runtime.ExecutorBackends()`
  hooks for future docker/gvisor backend registration at boot.

### Changed
- `tools/command.go` now delegates the actual process spawn to the
  backend the runtime injects via a reserved `__sandbox` key on the
  tool input (matching the existing `__on_delta` escape-hatch pattern).
  Validation (argv parsing, allowed_binaries, workspace_root,
  env allowlist) stays in the tool layer. Result map now includes a
  `"backend"` field naming which backend ran the command.

### Removed
- `internal/tools/sandbox_unix.go` + `sandbox_windows.go` — the
  platform-specific `sandboxSysProcAttr` helpers move into the new
  `executor` package alongside the local backend.

### Roadmap
- Roady feature "Sandboxed executor backends" — PR1 (this change)
  lays the interface + local default. PR2 will add the docker backend
  (rootless containers, bind-mounted workspace, CPU/mem limits,
  `network.egress` capability). PR3 adds gVisor (runsc) variant.
  PR4 adds the `network.egress` permission rule and Prometheus
  counters. PR5 adds the Tauri settings UI dropdown.

## [Unreleased] - 2026-05-22 (ADR 0004 revised — Mnemos is a plugin)

Course correction after discovering that real Mnemos
(`github.com/felixgeelhaar/mnemos`) is a knowledge-graph HTTP service
with a typed Go client at `mnemos/client`, not a yet-to-be-built
embedded memory library. The earlier extraction work has been
reverted; the integration redesign treats Mnemos as a plugin under
ADR 0001.

### Reverted
- `9731023 feat(api): migrate REST CRUD memory handlers to mnemos.Client`
  → revert commit `2f72e34`.
- `ba4ea22 feat(mnemos): consume external mnemos package (ADR 0004 step 2)`
  → revert commit `7967e77`. Restores `internal/mnemos` + the prior
  `internal/memory.EmbeddedClient`, removes the `go.mod` require +
  local replace directive, restores the `events.run_id` NOT NULL +
  FK constraints, and rolls migration `000024_events_optional_run_id`
  off the schema.

### Changed
- **ADR 0004** revised in place. Status remains Accepted but the
  decision rewrites from "extract Mnemos package + introduce
  `mnemos.Client` interface" to "integrate real Mnemos as a plugin
  per ADR 0001." Two-layer architecture clarified: local
  `internal/memory.Manager` for per-assistant scratch (unchanged) +
  optional Mnemos plugin for organizational knowledge graph (claims,
  evidence, relationships, visibility scopes).
- **`docs/index.html`** — softens the Mnemos prominence: the
  "Cognitive layer" primary card becomes "Persistent memory you can
  read" with Mnemos demoted to "drops in as an optional plugin." OG
  meta description drops the "memory-native" framing. Phase 4
  roadmap card explicitly frames Mnemos as a plugin. Built-on lib
  card describes real Mnemos shape (events / claims / evidence /
  relationships) instead of the imagined embedded library.
- **`README.md`** — same softening. Mnemos referenced as an optional
  plugin for team-scale structured knowledge; per-assistant scratch
  framed as local SQLite. Architecture diagram updates to "Local
  memory" instead of "Memory → mnemos."

### Removed
- The fake local Mnemos repo at
  `/Users/felixgeelhaar/Developer/projects/open_source/mnemos`
  (created during the misread; deleted manually).

### Roadmap
- Roady feature #113 — "Mnemos Plugin (ADR 0001 + ADR 0004 revised)"
  supersedes the now-stale feature #112. Tracks the actual plugin
  implementation: tools (`mnemos.events.append`,
  `mnemos.claims.append`, `mnemos.claims.list`,
  `mnemos.relationships.list`, `mnemos.embeddings.append`,
  potentially `mnemos.embeddings.similar`), context source, capability
  split (`mnemos.read` / `mnemos.write`), connection configuration via
  the existing `ConnectorConfigRepository`.
- Feature #112 (Mnemos package extraction) stays in the spec for
  history but is inactive — superseded by #113.

## [Unreleased] - 2026-05-22 (ADR 0004 step 1 — mnemos.Client extraction)

Implements step 1 of ADR 0004's migration path: runtime depends on the
`mnemos.Client` interface instead of `*memory.Manager`. Foundation for
step 2 (extract package to standalone Mnemos repo with its own SQLite
file) and step 3 (HTTP-backed `mnemos/remote`). Zero behavior change
for the laptop user; full repo test suite (40 packages) green under
`-race`.

### Added
- **`internal/mnemos`** — wire-shaped types and interface. `Scope`
  (owner_id, kind, key) replaces flat string scope; `ScopeKind` enum
  covers workspace/profile/session/org/preferences. `Client` interface:
  Store, Retrieve, Search, Forget, Tombstone. Sentinel errors
  (`ErrNotFound`, `ErrInvalidScope`, `ErrScopeMismatch`).
  `ValidateScope` enforces kind/key well-formedness. Helpers
  `LocalWorkspace()`, `LocalProfile()`, `LocalPreferences()`.
- **`internal/memory.EmbeddedClient`** — in-process implementation over
  the existing `*db.MemoryRepository`. SHA-256 `ContentHash` computed
  on Store. Scope-isolated Forget rejects cross-scope IDs as
  `ErrNotFound`. Tombstone routes to `AnonymizeByAssistant` /
  `AnonymizeByRun`. Optional `WithEventBus(bus)` enables audit
  emission.
- **Audit events** — new `EventMemoryStore`, `EventMemoryForget`,
  `EventMemoryTombstone` event types. `EmbeddedClient` emits with
  `content_hash` populated; events feed into the existing
  hash-chained audit log via the same `EventBus.Publish` path used by
  every other domain event.
- **Tombstone wiring** — new `EventAssistantDeleted` /
  `EventRunDeleted` event types. `Runtime.DeleteRun` and
  `api.AssistantServer.DeleteAssistant` publish them; runtime
  subscribes at boot in `startTombstoneSubscriber` and routes to
  `Client.Tombstone`. `AssistantServer` constructor extended to take
  an `*events.EventBus`.
- **`AnonymizeByAssistant` / `AnonymizeByRun`** on `MemoryRepository`
  — mirrors today's `ON DELETE SET NULL` FK behavior. Idempotent.
- **JSONL export/import** — `memory.Export(ctx, client, scope, w)` and
  `memory.Import(ctx, client, r)` with versioned header
  (`mnemos.export` v1). REST: `GET /memory/export?scope=&key=` streams
  JSONL with `application/x-ndjson` + Content-Disposition; `POST
  /memory/import` returns `{"imported": N}`. CLI: `nomi memory export
  [--scope] [--key] [-o file]` and `nomi memory import file|-`.
- **Migration `000024_events_optional_run_id`** — makes `events.run_id`
  nullable and drops the FK to `runs(id)`, enabling entity-scoped
  events (assistant/run deletion + memory ops) to persist. Down
  migration aborts if NULL rows present.

### Changed
- `Runtime.memManager *memory.Manager` → `Runtime.memClient
  mnemos.Client`. Constructor signature updated. All four runtime call
  sites (`lifecycle.go:257`, `engine.go:650+662`, `planner.go:104`)
  rewritten to use Client methods. `scopeFromPolicy()` helper maps
  legacy `MemoryPolicy.Scope` strings to typed `mnemos.Scope`.
- `events.validateEvent` relaxed via `isEntityScopedEvent` allowlist —
  events targeting an entity other than a run (assistant.deleted,
  run.deleted, memory.*) may carry empty RunID.
- `cmd/nomid/main.go` constructs both `*memory.Manager` (for the REST
  CRUD endpoints that haven't migrated yet) and `*memory.EmbeddedClient`
  (for the runtime + the new export/import endpoints).
- 9 test files updated: `memory.NewManager(repo)` →
  `memory.NewEmbeddedClient(repo)`; `setupTestRuntimeWithMemory` return
  type widened; one assertion `rt.memManager.ListByScope(...)` rewritten
  as `rt.memClient.Retrieve(ctx, mnemos.LocalWorkspace(), Query{})`.

### Tests
- `internal/mnemos/client_test.go` — 9 ValidateScope cases (happy
  paths, missing owner, unknown kind, kind/key mismatch).
- `internal/memory/client_test.go` — 14 cases: ID/CreatedAt/ContentHash
  assignment, round-trip, scope isolation, query filters
  (assistant/run/since), substring search, Forget happy/unknown/
  cross-scope, Tombstone assistant/run/idempotent, invalid-scope
  reject, context-cancel reject.
- `internal/memory/exportimport_test.go` — 5 cases: round-trip,
  unknown format reject, bad version reject, invalid scope in header,
  invalid scope on export.
- `internal/memory/audit_integration_test.go` — verifies
  memory.store/forget/tombstone events fire with `content_hash`.
- `internal/api/memory_export_test.go` — REST round-trip + 400 on
  invalid scope.

### Roadmap
- Roady feature #112 (next) — extract `internal/mnemos` +
  `EmbeddedClient` to `github.com/felixgeelhaar/mnemos` standalone
  repo; introduce separate `mnemos.db` SQLite file; data-migration on
  first boot post-upgrade (ADR 0004 step 2).

## [Unreleased] - 2026-05-22 (narrative & positioning)

Repositioning pass: Nomi reframed from "local-first coding agent" to
"personal AI runtime with persistent cognition (Mnemos)." Coding stays
as the flagship workflow; the runtime + capability engine + memory
architecture is now the category claim.

### Changed
- **Landing page (`docs/index.html`)** — new hero ("Your personal AI
  runtime"), problem section reframed around stateless-and-confident
  agents, diff grid restructured to "Runtime + capability engine +
  memory" with Mnemos promoted to a primary card and a new
  "Daemon-not-IDE-plugin" card, Compared section expanded.
- **`README.md`** — hero rewritten to match; "Why Nomi" leads with the
  runtime/cognitive-layer framing; Compared-to table extended.
- **`docs/comparison.md`** — new "Personal AI assistant category"
  section comparing Nomi to OpenClaw, NanoClaw, Hermes Agent (Nous
  Research), and Pi (Inflection AI) with feature matrix + per-product
  detail subsections. Threat-model row added.

### Added
- **ADR 0004 — Nomi ↔ Mnemos Cognitive Boundary** (Accepted 2026-05-22).
  Defines `mnemos.Client` interface, embedded vs remote implementations,
  separate-DB decision with event-driven cleanup for FK loss, scope
  isolation honesty (advisory in embedded, authoritative in remote),
  three-step migration path, audit-chain mitigation, performance and
  failure-mode budgets, acceptance criteria.
- **`docs/context-budget.md`** — auto-loaded files table, model
  windows, on-demand reference sizes. Current auto-load ~43K tokens
  (~22% of 200K window). Review cadence quarterly.
- **`docs/vendor-notes.md`** — append-only gotchas log: Tauri WKWebView
  SSE workaround, Vite port mismatch (5173 vs 4173), SQLite FK pragma
  trap, golang-migrate up+down pair requirement, Playwright needs
  `nomid` running, Gin CORS posture, Ollama port detection, shadcn
  primitives are vendored.

### Roadmap
- Roady feature #110 — "Repositioning: AI Runtime + Mnemos Cognitive
  Layer Narrative" tracks remaining narrative work (OG image verify,
  Phase 4 org-cognition copy, ADR 0004 acceptance).

## [Unreleased] - 2026-05-09 (post-v0.1 cycle 2)

Two rounds of expert review (product + ai expert agents) drove this
cycle. 9 features shipped end-to-end through the roady backlog.

### Added
- **Replan-on-failure loop** — `Runtime.Replan` re-prompts the planner
  with prior step outputs + the failure as a `<previous_attempts
  trusted="false">` block, bounded by `MaxReplansPerRun`. Wired both
  automatic (executor calls before falling back to `failRun`) and
  manual (`POST /runs/:id/replan` + "Fix this with the agent" CTA on
  failed runs in chat-detail). Closes the wedge gap with Claude Code /
  Cursor / Aider, which all iterate on test failures.
- **`filesystem.patch` hardening** — diff size cap (1 MB), structured
  `--- / +++` header parsing, on-disk pre-flight (modify/delete must
  exist; create-blocks must not collide), `git apply --check` dry-run
  with `-3 --whitespace=fix` 3-way fallback. New `UserError` codes:
  `patch_file_missing`, `patch_too_large`, `patch_apply_failed` so the
  replan loop can react with appropriate retry strategy.
- **Coding Agent as onboarding default** — wizard's quickstart now
  picks `coding-agent` over `code-reviewer`. `pickOllamaModel` biases
  toward `qwen2.5-coder` / `deepseek-coder` / `codellama` when the
  template wants a coder. README quickstart pulls `qwen2.5-coder:7b`.
- **Planner-context budget envelope** — `summarizePriorAttempts` lives
  in `internal/runtime/context_window.go` with two-level budgets:
  `StepOutputBudget` (512 B per step) + `PriorAttemptsBudget` (8 KB
  total) with recency-biased truncation and an explicit `[N earlier
  step(s) elided]` breadcrumb so the planner knows context is partial.
- **Real provider labels** — `llm.Client.Provider() string` discriminates
  openai / anthropic / ollama / openai-compat from baseURL. Threaded
  through `metrics.PlannerCallsTotal` so each backend gets its own
  series.
- **`nomi_planner_edit_distance_total{provider, edit_kind}`** —
  counts step titles added vs removed when the user edits a plan; the
  leading indicator of planner quality drop.
- **Adversarial planner evals + threshold gate** —
  `internal/runtime/evals/planner_adversarial_test.go` covers
  markdown-fenced JSON, prose-wrapped JSON, unknown-tool hallucination.
  `NOMI_GOLDEN_THRESHOLD` is now an enforced gate (was dead code that
  only logged). New `make eval-live` runs golden + adversarial corpus.
- **Chat-list run search** — case-insensitive substring filter on
  `ChatItem.title` with empty-state messaging. Server-side
  `RunRepository.Search` + `GET /runs?search=<q>` available for once
  the corpus outgrows client-side filtering.
- **Branch-from-here CTA** — completed/failed runs in chat-detail
  expose a button that calls the existing fork endpoint with the last
  step ID.
- **DiffPreview parity overhaul** — structured per-file / per-hunk
  parser, per-hunk Skip / Include toggle (rebuilds the diff payload
  via `onDiffChange` callback), Unified ↔ Side-by-side toggle
  persisted to `localStorage`. Class names already match what a
  future Shiki migration will reuse.
- **HN/launch FAQ** — `docs/launch/hn-faq.md` pre-stages the first
  comment for a public launch (privacy posture, prompt-injection
  handling, replan loop, what's still rough).

### Changed
- Memory search is now case-insensitive token-aware substring match
  (`strings.Contains(strings.ToLower(...), token)` over each
  whitespace-delimited query token). Until SQLite FTS5 lands, this is
  the cheapest fix that prevents `"auth"` from missing capitalised
  rows.
- README + `docs/comparison.md` purged of stale "planner emits one
  hardcoded step" / "one-step plans for non-LLM intents" claims —
  multi-step LLM-generated plans + replan have shipped.
- ApprovalPanel is now a list-only deep-link surface; the in-context
  ApprovalCard inside chat-detail is the single source of truth for
  approve/deny. Pre-refactor the same approval was resolvable from
  two places, leading to stale state.
- ApprovalCard color tokens swapped to semantic destructive / amber+dark
  pairs so light and dark themes both pass WCAG AA contrast (no more
  `text-red-700` on `bg-red-50`). `aria-live="assertive"` reserved for
  irreversible-only; routine approvals use `polite` so screen readers
  don't get spammed.
- Runtime CAS-style transitions (`runRepo.CASUpdateStatusTx`) prevent
  two writers racing the same `RunCreated → RunPlanning` transition.

### Fixed
- Pre-existing JSX/TS errors in `assistant-manager.tsx` (Memory scope
  + Permissions sections lost during a Select-component migration).
- 26c9b7d corrupted half the form during a shadcn-Select migration;
  restored verbatim from the parent commit. CI lint now passes clean.

## [0.1.0] - 2026-05-01

First public beta. Local-first, state-driven agent platform.

### Added
- **Runtime engine** with `Run` / `Plan` / `Step` state machines
  (`pkg/statekit`) and capability-gated tool execution.
- **Permission engine** with `allow` / `confirm` / `deny` modes,
  remembered approvals, declared-capability ceiling, and TOCTOU
  re-check at the tool boundary.
- **Core tools**: `llm.chat` (streaming-capable), `filesystem.read`,
  `filesystem.write`, `filesystem.context`, `command.exec`. Per-tool
  argument schema validates planner output before persistence.
- **Plugin architecture** (ADR 0001) with cardinality-aware
  connections, multi-turn conversations, identity allowlisting, and
  WASM marketplace plugins (filesystem / command / secrets / HTTP host
  bridges).
- **Bundled plugins**: Telegram, Email (IMAP/SMTP), Slack, Discord,
  Gmail, Calendar, GitHub, Obsidian, Browser (Scout), Media (Piper TTS
  + whisper.cpp STT).
- **LLM integration**: OpenAI-compatible + Anthropic native adapters,
  per-assistant model overrides, streaming token output via SSE,
  endpoint normalisation (`/v1` auto-append), provider probe endpoint
  for pre-save validation.
- **Persistence**: SQLite with embedded migrations, per-step argument
  storage, branched runs, hash-chained event audit log, rotating
  bearer-token auth.
- **REST API + SSE event stream** on `:8080`. Tauri shell consumes
  events through native IPC bridge with auto-reconnect.
- **Desktop UI** (React 19 + Tauri 2 + shadcn/ui): chats, assistants,
  approvals, memory, events, plugins, AI providers, safety profiles,
  about. macOS menu-bar tray with idle / active / awaiting states.
  Onboarding wizard.
- **22 end-to-end user journeys** (`test/journeys/run.sh`) covering
  first-run, Q&A, file write with approval, plan editing, branching,
  pause/resume, memory persistence, late policy deny (TOCTOU close),
  endpoint hardening, audit export, provider rotation, manual
  provider/assistant CRUD, plugin enable/disable, multi-assistant
  routing, per-assistant model override, event stream consistency,
  provider probe, auth-token rotation, streaming `llm.chat`. All
  passing against real Ollama (`qwen2.5:14b`).
- **Distribution**: Homebrew Cask (`brew install --cask
  felixgeelhaar/tap/nomi`), Scoop (`scoop install nomi`), Docker
  (headless `nomid` daemon), `go install
  github.com/felixgeelhaar/nomi/cmd/nomid@latest`.

### Security
- Auth-token rotation HTTP endpoint with atomic in-memory store; old
  token invalidated immediately.
- Provider endpoint validation rejects non-http(s) schemes, missing
  hosts, and `file://` / `javascript:` / `gopher://` URLs.
- Planner argument allowlist prevents a poisoned LLM from injecting
  reserved keys (`workspace_root`, `system_prompt`, `allowed_binaries`,
  …) into tool input.
- Approval signature hashes the resolved tool arguments, not the
  natural-language step description, so a remembered "approve writes
  to notes.md" doesn't apply to a later step with `path=/etc/hosts`.
- Migration `000018` backfills `llm.chat` capability for legacy
  assistants without elevating any pre-existing `*=deny` rules.
- WASM host imports gate every call through the assistant's policy
  before forwarding to the tool executor.

## [0.1.1] - 2026-05-08

Quality and UX hardening release focused on approvals, chat ergonomics,
auth resilience, and UI consistency.

### Added
- **Approval UX hardening** with plain-English summaries, pending approval
  screen-reader announcements (`role="alert"`, `aria-live="assertive"`),
  and unified approval visual treatment between chat-inline approvals and
  the approvals panel.
- **New shadcn Select primitive** (`app/src/components/ui/select.tsx`) and
  migration of key assistant-selection/model-selection controls off native
  `<select>` to align focus/keyboard behavior with the rest of the design
  system.

### Changed
- **Chat input ergonomics**: IME-safe enter handling, multiline semantics,
  scroll-position awareness, debounced auto-scroll, and "Scroll to bottom"
  affordance for long-running conversations.
- **Plugin UI refresh behavior**: moved from aggressive 5s/10s polling to
  event-driven invalidation with 60s safety-net intervals.
- **Dependency stability**: pinned `lucide-react` to exact version `1.14.0`
  to avoid drift from unreviewed minor updates.

### Fixed
- **Final response rendering bug**: final LLM conclusion is now rendered as
  the chat bubble output instead of being obscured by the thinking block.
- **Auth cache invalidation on 401**:
  - LLM clients now surface typed auth failures (`AuthError`),
  - resolver cache invalidates on auth errors,
  - tool path invalidates provider cache on auth failures,
  - Google OAuth/Gmail paths invalidate account auth state on 401.

[0.1.1]: https://github.com/felixgeelhaar/nomi/releases/tag/v0.1.1

[0.1.0]: https://github.com/felixgeelhaar/nomi/releases/tag/v0.1.0
