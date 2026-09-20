# Nomi VS Code / Cursor extension

Thin client for [Nomi](https://github.com/klarlabs-studio/nomi) — approve
plans and tool calls from the editor without rebuilding the desktop UI.

## What it does

- Status-bar badge with pending **tool approvals** + **plan_review** runs
- Quick Pick to Approve / Deny (plan deny = cancel, same as tray/channels)
- Auto-discovers `auth.token` + `api.endpoint` from the Nomi data dir
  (same paths as `nomi` CLI / Tauri)

## Requirements

- `nomid` running on this machine (or a reachable host with token override)
- VS Code ≥ 1.85 or Cursor

## Install (dev)

```bash
cd extensions/vscode
npm ci
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
| `nomi.pollIntervalMs` | Badge refresh when idle (default 15s) |

## Out of scope (v1)

Chat, plan edit, DiffPreview, MCP settings — use the Tauri desktop app.
SSE live push lands in a follow-up; v1 polls.
