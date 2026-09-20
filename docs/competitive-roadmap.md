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
- Docker/gVisor + DNS/eBPF egress, signed WASM, Mnemos FTS5, `/audit/verify`

## Where we still lose

| Competitor | Gap |
|---|---|
| OpenClaw | Messaging long-tail + “just do it” friction |
| Goose | MCP env map for GitHub/Postgres-style secrets |
| Cline | Richer in-editor plan/diff UI (we send context; DiffPreview stays desktop) |
| Claude Code | Terminal-native Plan→Diff→Approve loop |
| Hermes | Pocket-first always-on + OpenRouter model menu |

## Next moves (priority)

1. **OpenRouter provider** (`cursor/openrouter-provider-4135`) — Hermes/Goose model menu
2. **Discord + WhatsApp plan review** (`cursor/discord-whatsapp-plan-review-4135`)
3. **CLI plan/diff/approve loop** (`cursor/cli-plan-approve-loop-4135`) — Claude Code refugees
4. **Skills + schedules polish** (`cursor/skills-schedules-polish-4135`) — Hermes always-on, inspectable
5. **MCP env map** (follow-up to presets) — GitHub / Postgres servers that need secrets in env
6. **Extension SSE** — live badge without polling

## Defer

OpenClaw connector long-tail, hosted Mnemos, cross-device sync, native
mobile apps, Pi-style companion, micro-VM isolation race.
