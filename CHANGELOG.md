# Changelog

All notable changes to Nomi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [Unreleased] - 2026-05-22 (REST CRUD memory on mnemos.Client)

Closes the deferred follow-up from step 2: REST CRUD memory endpoints
(`POST/GET/DELETE /memory`) now flow through `mnemos.Client` instead
of `*memory.Manager`, so the desktop UI's Memory tab reads/writes the
same store the runtime writes to. The legacy `*memory.Manager` stays
only as a one-shot migration source.

### Changed
- **`internal/api/memory.go`** — `MemoryServer` rewritten over
  `mnemos.Client`. `*memory.Manager` field removed. New response
  shape `memoryResponse` decouples the wire format from
  `mnemos.Entry` so upstream field renames don't ripple through the
  API surface.
- **ID-based endpoints** (`GET /memory/:id`, `DELETE /memory/:id`)
  iterate `idLookupScopes` (workspace, profile, preferences) because
  the path carries no scope hint. Workspace-first since most writes
  land there. Worst-case 3 lookups on a miss.
- **`ListMemory`** drops the implicit "workspace + profile" union
  when no scope is given — now defaults to workspace only. Callers
  that want both pass `?scope=...` explicitly.
- **`RouterConfig.Memory *memory.Manager`** field removed; only
  `MemoryClient mnemos.Client` remains. `NewMemoryServer(client)`
  takes one arg.
- **`cmd/nomid/main.go`** renames `memManager → legacyManager` to
  signal its single remaining role (input to
  `MigrateLegacyMemory`). Passed through `RouterConfig.MemoryClient`
  is the embedded backend; the legacy manager never reaches the
  router.

### Upstream
- `github.com/felixgeelhaar/mnemos` commit `1afbc79` — added
  `Client.GetByID(ctx, scope, id) (*Entry, error)` to the interface
  and the embedded implementation. Scope-isolated (same posture as
  Forget). 3 new tests (happy, unknown id, cross-scope).

## [Unreleased] - 2026-05-22 (ADR 0004 step 2 — Mnemos package extracted)

Step 2 of ADR 0004's migration path. The Mnemos package + the
embedded SQLite backend now live in the standalone
`github.com/felixgeelhaar/mnemos` repository. Nomi consumes them via
`go.mod`. Memory persists to a separate `mnemos.db` SQLite file
alongside `nomi.db`; first-boot migration copies the legacy memory
table over once.

### Added
- **`internal/memory/emitter.go`** — `BusEmitter` adapts the standalone
  `embedded.Emitter` interface to Nomi's `*events.EventBus`. Memory
  ops emitted by the external package land in the existing
  hash-chained audit log via this thin shim.
- **`internal/memory/migrate.go`** — `MigrateLegacyMemory(ctx, db,
  dst, src)` reads every row from `nomi.db`'s legacy `memory` table
  and stores it through the new mnemos.db backend. Records completion
  in `app_settings.mnemos_legacy_migration_completed_at` so subsequent
  boots short-circuit. Idempotent; preserves the legacy table for
  rollback safety. 3 unit tests (copy + idempotent + empty-noop).
- **`internal/memory/testhelpers.go`** — `NewTestClient(t)` returns an
  in-memory `*embedded.Client` registered for cleanup. Replaces the
  dozen-plus call sites that previously constructed
  `*memory.EmbeddedClient`.

### Changed
- **`go.mod`** — requires `github.com/felixgeelhaar/mnemos` v0.0.0
  (replace directive pins to local `../mnemos` until v0.1.0 is tagged
  upstream).
- **`cmd/nomid/main.go`** — opens `<dataDir>/mnemos.db` via
  `embedded.Open`, attaches `memory.NewBusEmitter(eventBus)`, invokes
  `memory.MigrateLegacyMemory` once at boot. The `*memory.Manager`
  used by REST CRUD endpoints (POST/GET/DELETE /memory) continues to
  write to the legacy `nomi.db` memory table — its migration is a
  follow-up.
- **`internal/api/memory.go`** — `ExportMemory` and `ImportMemory`
  call `embedded.Export` / `embedded.Import` (relocated from
  `internal/memory`).
- All runtime test files (`runtime`, `runtime/evals`, `api/smoke`)
  swap `memory.NewEmbeddedClient(repo)` for `memory.NewTestClient(t)`.
- `setupTestRuntimeWithMemory` returns `mnemos.Client` instead of
  `*memory.EmbeddedClient`.

### Removed
- `internal/mnemos/` — moved upstream to
  `github.com/felixgeelhaar/mnemos`.
- `internal/memory/client.go`, `client_test.go`, `exportimport.go`,
  `exportimport_test.go`, `audit_integration_test.go` — all moved
  upstream (the equivalent tests live in
  `github.com/felixgeelhaar/mnemos/embedded`).

### Mnemos repository
- Initial commit `e913fbf` bootstrapped at
  `github.com/felixgeelhaar/mnemos`:
  - Root package: `Client` interface, `Scope`/`Entry`/`Query`/
    `EntityRef` types, `ValidateScope`, sentinel errors. Stdlib only.
  - `embedded` subpackage: SQLite-backed implementation with
    `Open(path)`, in-line DDL bootstrap (no external migrate tool),
    optional `Emitter` for audit emission, JSONL `Export`/`Import`.
  - 9 ValidateScope tests, 14 embedded.Client tests (incl. Emitter
    fan-out + Close idempotency), 4 export/import tests. All green
    on Go 1.22.x + 1.23.x.
  - Apache 2.0, GitHub Actions CI, README documenting the contract.

### Roadmap
- Push `github.com/felixgeelhaar/mnemos` to GitHub, tag v0.1.0, drop
  the local replace directive from Nomi's `go.mod`.
- Migrate REST CRUD memory handlers (`POST/GET/DELETE /memory`) to
  use `mnemos.Client` so the desktop UI's Memory tab reads/writes
  the same store the runtime writes to. Add `GetByID` to the
  `mnemos.Client` interface in the same change.
- Roady feature #113 (planned) — `mnemos/remote` HTTP client (ADR
  0004 step 3).

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
