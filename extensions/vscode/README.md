# Nomi VS Code / Cursor extension

Thin client for [Nomi](https://github.com/klarlabs-studio/nomi) — approve
plans and tool calls from the editor without rebuilding the desktop UI.

## What it does

- Status-bar badge with pending **tool approvals** + **plan_review** runs
- **Live SSE** on `/events/stream` — badge refreshes on `approval.*` /
  `plan.*` / `run.cancelled` (15s poll fallback when the stream drops)
- **Plan Review panel** — steps + Shiki-highlighted DiffPreview (unified /
  side-by-side toggle); uncheck steps or hunks to edit via `/plan/edit`
  (CLI `--review` [E]dit parity); Approve / Deny without leaving the editor
- Quick Pick for tool approvals (plan deny = cancel, same as tray/channels)
- **Ask Nomi** — editor context menu + command palette +
  `Ctrl/Cmd+Shift+Alt+N`; starts a run with open tabs + active selection
  attached (paths only; secrets filtered; selection ≤ 4 KiB)
- Keyboard shortcuts for Review plan, Show pending, Approve, Deny
  (`…+R` / `…+P` / `…+Y` / `…+D`)
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

## Keybindings

| Shortcut | Command |
|---|---|
| `Ctrl/Cmd+Shift+Alt+N` | Ask Nomi (editor focused) |
| `Ctrl/Cmd+Shift+Alt+R` | Review pending plan |
| `Ctrl/Cmd+Shift+Alt+P` | Show pending approvals & plans |
| `Ctrl/Cmd+Shift+Alt+Y` | Approve selected |
| `Ctrl/Cmd+Shift+Alt+D` | Deny selected |

Remap under Keyboard Shortcuts if they collide with other extensions.

## DiffPreview chrome

Plan Review highlights patch hunks with **Shiki** in the extension host
(injected HTML — webview CSP cannot load WASM) and offers a **Side-by-side**
toolbar toggle (preference sticky via `webview.setState`). Click a file
label to open it in the editor. Write / shell steps still show plain
excerpts; open the Tauri app for the full chat surface.
