# Changelog

All notable changes to Nomi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

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
