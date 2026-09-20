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
- Docker/gVisor + DNS/eBPF egress, signed WASM, Mnemos FTS5, `/audit/verify`

## Where we still lose

| Competitor | Gap |
|---|---|
| OpenClaw | Messaging long-tail + “just do it” friction |
| Goose | MCP setup feels lighter (presets / one-click) |
| Cline | Zero context-switch inside the editor |
| Claude Code | Terminal-native Plan→Diff→Approve loop |
| Hermes | Pocket-first always-on + OpenRouter model menu |

## Next moves (priority)

1. **MCP one-click presets** (`cursor/mcp-presets-catalog-4135`) — Goose UX parity
2. **VS Code / Cursor thin client** (`cursor/vscode-thin-client-4135`) — Cline switch lever
3. **Editor context injection** (`cursor/editor-context-inject-4135`) — open files / selection into planner
4. **OpenRouter provider** (`cursor/openrouter-provider-4135`) — Hermes/Goose model menu
5. **Discord + WhatsApp plan review** (`cursor/discord-whatsapp-plan-review-4135`)
6. **CLI plan/diff/approve loop** (`cursor/cli-plan-approve-loop-4135`) — Claude Code refugees
7. **Skills + schedules polish** (`cursor/skills-schedules-polish-4135`) — Hermes always-on, inspectable

## Defer

OpenClaw connector long-tail, hosted Mnemos, cross-device sync, native
mobile apps, Pi-style companion, micro-VM isolation race.
