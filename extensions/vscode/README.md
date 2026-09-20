# Nomi VS Code / Cursor extension

Thin client for [Nomi](https://github.com/klarlabs-studio/nomi) — approve
plans and tool calls from the editor without rebuilding the desktop UI.

## What it does

- Status-bar badge with pending **tool approvals** + **plan_review** runs
- **Live SSE** on `/events/stream` — badge refreshes on `approval.*` /
  `plan.*` / `run.cancelled` (15s poll fallback when the stream drops)
- **Plan Review panel** — steps + unified diffs / write excerpts; uncheck
  steps to drop via `/plan/edit` (CLI `--review` [E]dit parity); Approve /
  Deny without leaving the editor
- Quick Pick for tool approvals (plan deny = cancel, same as tray/channels)
- **Ask Nomi** — editor context menu + command palette; starts a run with
  open tabs + active selection attached (paths only; secrets filtered;
  selection ≤ 4 KiB)
- Auto-discovers `auth.token` + `api.endpoint` from the Nomi data dir
  (same paths as `nomi` CLI / Tauri)

## Requirements

- `nomid` running on this machine (or a reachable host with token override)
- VS Code ≥ 1.85 or Cursor
- At least one assistant (or set `nomi.defaultAssistantId`)

## Install (dev)

```bash
cd extensions/vscode
npm ci --legacy-peer-deps
npm run compile
# In VS Code/Cursor: Extensions → Install from VSIX… after `npx @vscode/vsce package`
# Or: F5 from this folder with the Extension Development Host
```

## Settings

| Setting | Purpose |
|---|---|
| `nomi.apiUrl` | Override base URL |
| `nomi.token` | Override bearer (prefer `$NOMI_TOKEN`) |
| `nomi.dataDir` | Override data directory |
| `nomi.pollIntervalMs` | Badge refresh fallback when SSE is down (default 15s) |
| `nomi.defaultAssistantId` | Skip assistant Quick Pick on Ask Nomi |

## Out of scope (v1)

Per-hunk skip, Shiki highlighting — use the Tauri desktop DiffPreview.
