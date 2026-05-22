# Context Budget

Tracks files that get auto-loaded into Claude Code's context on every session, plus the heavy reference files that get pulled in on demand. Goal: keep auto-loaded context under ~30% of the model's window so there's room for actual work.

**Last reviewed:** 2026-05-21

## Model windows

| Model | Window | 30% target |
|---|---|---|
| Opus 4.7 (`claude-opus-4-7`) | 200K | 60K |
| Opus 4.7 1M variant (`claude-opus-4-7[1m]`) | 1M | 300K |
| Sonnet 4.6 | 200K | 60K |

## Auto-loaded files

Loaded on every session start before the user types anything.

| File | Lines | Est. tokens | Notes |
|---|---:|---:|---|
| `~/.claude/CLAUDE.md` | 380 | ~15K | User global. Heavy — covers all projects. Trim candidate if it grows. |
| `CLAUDE.md` (project root) | 101 | ~3K | Project-specific. Tight. Don't grow past ~150 lines. |
| `~/.claude/projects/<proj>/memory/MEMORY.md` | 1 | <100 | Auto-memory index. Capped at 200 lines by the system. |
| System prompt + tool schemas + skill list | — | ~25K | Not in our control. Grows with installed MCP servers / skills. |

**Estimated auto-loaded total: ~43K tokens** (~22% of 200K window, ~4% of 1M window).

## On-demand reference files

Not auto-loaded, but pulled in on most planning sessions. Tracked because they're large enough to dominate a single turn.

| File | Lines | Est. tokens | When read | Mitigation |
|---|---:|---:|---|---|
| `.roady/spec.yaml` | 1517 | ~60K | Spec review, feature add | Read offset+limit; never read whole file |
| `.roady/state.json` | 2594 | ~50K | Task status checks | Use roady MCP tools, not Read |
| `.roady/plan.json` | 2364 | ~45K | Plan inspection | Use roady MCP tools, not Read |
| `internal/runtime/engine.go` | ~1000 | ~12K | Runtime work | Modularization feature pending in spec |
| `docs/index.html` | 327 | ~8K | Landing page edits | Fine |
| `README.md` | ~500 | ~12K | README edits | Fine |

## Rules of thumb

- **Never** read `.roady/spec.yaml` whole — always offset+limit or use `roady_get_spec`. Same for `state.json` and `plan.json`.
- ADRs in `docs/adr/` should stay under ~400 lines each. Longer = split into follow-up ADR.
- If `CLAUDE.md` (project) approaches 150 lines, move detail into `docs/architecture.md` and reference it.
- If `~/.claude/CLAUDE.md` (global) approaches 500 lines, audit for project-specific bleed.
- Auto-memory entries (in `memory/` dir) should be one focused fact per file; index line ≤150 chars.

## Review cadence

Every quarter or when a new heavy reference lands. Update the "Last reviewed" date and the line/token columns. If auto-loaded total crosses 50K on a 200K model, trim before adding anything new.
