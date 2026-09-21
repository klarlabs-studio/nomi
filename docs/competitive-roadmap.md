# Competitive roadmap (2026-09-20)

How Nomi becomes the agent power users switch *to* from Goose, Cline,
OpenClaw, Claude Code, and Hermes — not a feature laundry list.

## Thesis

Goose’s MCP catalog + Cline’s editor presence + OpenClaw’s channel reach,
**but** every irreversible step still goes through Nomi’s plan review,
per-tool capabilities, sandbox, and hash-chained audit.

## Already shipped (wedge)

- Plan-before-act as a real state machine (`plan_review`)
- Per-assistant capability engine + out-of-band approvals
- Generic MCP + per-tool caps (`mcp.<conn>.<tool>`) — #32
- Tray tool quick-approve; tray plan approve — #33
- Telegram/Slack channel plan approve — #35
- MCP one-click presets (Filesystem / Memory / Fetch / Git / …) — #37
- VS Code / Cursor thin client (`extensions/vscode`) — badge + Approve/Deny
- Editor context injection — open tabs + selection on `POST /runs`
- OpenRouter provider preset + attribution headers — #40
- Discord + WhatsApp channel plan approve (Message Components /
  interactive reply buttons) — #41
- CLI `nomi run --review` Plan→Diff→Approve loop — #42
- Skills + schedules inspectability (`nomi list schedules|skills`,
  schedule last-run deep-link, skill source-run links) — #43
- MCP env map (stdio secret env + GitHub/Postgres presets) — #44
- Extension SSE live badge (`/events/stream` + poll fallback) — #45
- MCP catalog UX polish (search, categories, runtime badges, create
  guards, empty-state CTA) — #46
- In-editor Plan Review (read-only steps + diffs; Approve/Deny) — #47
- CLI `--review` step drop via `/plan/edit` — #48
- Extension Plan Review step drop (checkboxes → `/plan/edit`) — #49
- Ask Nomi editor context menu — #50
- In-editor hunk skip on Plan Review (`filesystem.patch`) — #51
- CLI `--review` hunk skip via `/plan/edit` — this PR
- Docker/gVisor + DNS/eBPF egress, signed WASM, Mnemos FTS5, `/audit/verify`

## Where we still lose

| Competitor | Gap |
|---|---|
| OpenClaw | Messaging long-tail + “just do it” friction |
| Goose | Remote/synced marketplace catalog (local browse shipped) |
| Cline | Shiki / side-by-side DiffPreview polish (hunk skip shipped) |
| Claude Code | Full TUI (CLI can drop steps + skip hunks; rich TUI deferred) |
| Hermes | Pocket-first mobile UX (schedules/skills now inspectable from CLI + UI) |

## Next moves (priority)

1. **Channel auto-approve for safe-only plans** (opt-in) — OpenClaw “just do it” without skipping DiffPreview for writes
2. **Extension keybindings** — Cline muscle memory for Ask Nomi / Review

## Defer

OpenClaw connector long-tail, hosted Mnemos, cross-device sync, native
mobile apps, Pi-style companion, micro-VM isolation race,
remote/synced Goose-style MCP marketplace,
full in-editor DiffPreview chrome (Shiki / side-by-side),
full Claude Code TUI.
