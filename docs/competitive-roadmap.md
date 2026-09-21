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
- CLI `--review` hunk skip via `/plan/edit` — #52
- Opt-in auto-approve for safe messaging plans — #53
- Extension keybindings (Ask Nomi / Review / Approve / Deny) — #54
- Extension Shiki + side-by-side DiffPreview chrome — #55
- Email channel plan Approve/Deny (reply APPROVE/DENY) — #56
- CLI live step progress during `nomi run` — #57
- Extension open path from Plan Review DiffPreview — #58
- Extension live step progress (OutputChannel via SSE) — #59
- CLI `nomi review` attach to pending plan_review — #60
- CLI `nomi approve` / `nomi deny` for tool approvals — #61
- Desktop DiffPreview click-to-open path — #62
- CLI `nomi cancel` + Ctrl+C cancels active run — #63
- Extension Cancel active run (Ask Nomi / Plan Review Stop) — #64
- Extension auto-open Plan Review on `plan.proposed` — #65
- CLI `nomi pause` / `nomi resume` — #66
- Extension Pause / Resume active run — this PR
- Docker/gVisor + DNS/eBPF egress, signed WASM, Mnemos FTS5, `/audit/verify`

## Where we still lose

| Competitor | Gap |
|---|---|
| OpenClaw | Matrix / Teams / Signal / iMessage / Beeper (not one-PR each) |
| Goose | Remote/synced marketplace catalog (local browse shipped) |
| Cline | (closed) DiffPreview + Cancel + Pause/Resume + auto-open Plan Review |
| Claude Code | Full TUI deferred; CLI review/approve/deny/cancel/pause/resume + live progress; extension Cancel/Pause/Resume |
| Hermes | Pocket-first mobile UX (schedules/skills now inspectable from CLI + UI) |

## Next moves (priority)

1. **OpenClaw long-tail connectors** — only if channel demand justifies
   a dedicated connector (Matrix / Teams / Signal / iMessage / Beeper)

## Defer

OpenClaw connector long-tail (Matrix/Teams/Signal/iMessage/Beeper),
hosted Mnemos, cross-device sync, native mobile apps, Pi-style companion,
micro-VM isolation race, remote/synced Goose-style MCP marketplace,
full Claude Code TUI.
