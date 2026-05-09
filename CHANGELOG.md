# Changelog

All notable changes to Nomi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

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
