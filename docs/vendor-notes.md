# Vendor Notes

Cross-cutting gotchas with tools and libraries Nomi depends on. Not architecture, not ADRs — just "this surprised someone, don't get bitten again." Append-only; entries dated.

## Tauri v2

### WKWebView SSE limitation (macOS) — 2026-04

The browser's `EventSource` API misbehaves inside Tauri's WKWebView on macOS: connections hang on backgrounded windows, reconnect logic doesn't fire reliably. Symptom is the Event Log showing "Live" briefly then silently stopping.

**Workaround in production today:** the Rust side opens the SSE connection with `reqwest` and forwards each event via `window.emit()`. React subscribes through `useTauriEvents`, not `EventSource`. See `app/src-tauri/src/main.rs` `start_event_stream` command.

**Gotcha if you bypass the bridge:** running the React app via plain `vite preview` (no Tauri shell, e.g. for Playwright e2e at `:4173`) has no `window.emit` source. The api.ts client falls back to polling. Don't read "Live" status from inside that environment.

### `app-handle` lifetime — 2026-04

Holding an `AppHandle` across an async boundary requires `clone()` because of Send/Sync constraints. Symptom is a compile error pointing at the `tokio::spawn` call.

## Vite + React

### Dev server vs preview port mismatch — 2026-04

`npm run tauri dev` uses Vite dev server on `:5173`. Playwright e2e (`app/e2e/`) targets `vite preview` on `:4173`. Tests written assuming `:5173` will pass locally during dev and silently fail in CI.

## Go / SQLite

### `github.com/mattn/go-sqlite3` + WAL + FK pragmas — 2026-04

Foreign keys are **off by default** in SQLite. Nomi enables them via DSN: `_foreign_keys=1&_journal_mode=WAL`. If you connect from any other code path (a one-shot migration script, a test that opens the DB raw), FK enforcement and the WAL mode don't carry over and tests behave differently than runtime. See `internal/storage/db/db.go` for the canonical DSN.

### Embedded migrations + `golang-migrate` — 2026-04

Migrations are embedded via `//go:embed migrations/*.sql` and run on `nomid` startup. Adding a migration without **both** `NNNNNN_name.up.sql` AND `NNNNNN_name.down.sql` causes the embed FS to load but the migrator to reject the version as "dirty." Always add both files even if down is a single `-- noop`.

### `database.Migrate()` is the boot gate — 2026-04

Migrations run **before** any repository constructor. If a constructor reads a column added by a migration that hasn't been applied yet, you'll see "no such column" at boot. Order matters: migrations first, repositories second, see `cmd/nomid/main.go`.

## Playwright

### Tests need `nomid` running separately — 2026-04

`make app-build` then `npm run test:e2e` does **not** start `nomid`. Tests assume `:8080` is already serving. CI starts the daemon explicitly in a background step; locally, run `make dev` in another terminal first.

### Trivially-green tests — pre-2026-05-09

Older tests wrapped every assertion in `.catch(() => false)`. They passed regardless of whether the UI worked. The e2e-test-auth-bridge feature in `.roady/spec.yaml` replaces the surviving offenders. If you write a new test, don't import that pattern.

## Gin

### CORS is permissive for Tauri — 2026-04

`api/router.go` sets `Access-Control-Allow-Origin: *`. This is intentional for the local-Tauri case (the Tauri webview origin is `tauri://localhost`, not configurable). **Do not** copy this CORS posture into the headless-deployment doc — the headless guide assumes the operator puts a reverse proxy in front of `nomid` with tighter CORS.

## golang-migrate

### `-i` interactive flag not supported — ongoing

The MCP `git rebase -i` / `git add -i` style doesn't apply here, but golang-migrate's CLI uses `migrate -path ... -database ...` and has no interactive mode either. Always pass explicit version targets when scripting.

## Ollama

### Local endpoint port — 2026-04

Default is `:11434`. The First-Run Wizard (Phase 2 feature) probes this. If a user has changed it (uncommon), the probe falls through to "Ollama not detected" — they have to enter the endpoint manually. Documented in the wizard but easy to miss.

## shadcn/ui

### Components copied, not installed — ongoing

shadcn primitives live in `app/src/components/ui/`. They are **source-controlled in this repo**, not pulled from a package. Upstream fixes don't auto-propagate — when shadcn ships a fix, we re-paste manually. Track which primitives are in use before mass-upgrading.

---

**Appending:** add a `## <Tool>` heading if new, or a `### <Issue> — <YYYY-MM>` subsection if the tool already has notes. Keep each entry short — point at code or docs for detail.
