# Architecture

Nomi is an integrator. The interesting parts are four small Go libraries
released independently — `statekit`, `mnemos`, `scout`, `roady` — and
the application that wires them together with a permission engine, a
plugin host, and a desktop shell.

If you're shipping anything agentic, you can use any of these libraries
without adopting the whole stack.

## System diagram

```
┌─────────────────────────────────────────────────────────────┐
│                  Nomi.app (Tauri shell)                     │
│  React 19 + shadcn/ui · IPC bridge · macOS menu-bar tray    │
└──────────────────────────┬──────────────────────────────────┘
                           │  REST + SSE (Authorization: Bearer …)
┌──────────────────────────▼──────────────────────────────────┐
│                 nomid (Go runtime daemon)                   │
│  ┌────────────────────────────────────────────────────┐     │
│  │  Run / Plan / Step state machines  →  statekit     │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  Permission engine + approval workflow             │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  Tool registry · LLM resolver · Memory  →  mnemos  │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  Plugin registry · Browser  →  scout               │     │
│  │  WASM host (wazero)                                │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  Event bus  →  SSE stream + persisted audit log    │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  SQLite (WAL) · embedded migrations · OS keyring   │     │
│  └────────────────────────────────────────────────────┘     │
└──────────────────────────┬──────────────────────────────────┘
                           │  OpenAI-compat / Anthropic / Ollama
                           ▼
                    LLM provider(s)
```

## The cognitive-stack libraries

### [`statekit`](https://github.com/felixgeelhaar/statekit) — state

Type-safe finite state machines for Go, with XState JSON
compatibility. Every Nomi `Run`, `Plan`, and `Step` transition flows
through a statekit machine — including the approval pause/resume loop
that pivots execution on user input.

If your agent has phases — pending, planning, awaiting approval,
running, retrying, failed — you have a state machine. Boolean flags
will lie to you on day 90; statekit won't. Start with statekit when
the bug reports begin to read "the run is stuck in `executing` but
nothing is happening."

### [`mnemos`](https://github.com/felixgeelhaar/mnemos) — memory

Evidence-backed local-first knowledge engine. Memory entries are
claims with provenance, not opaque vector blobs. SQLite-backed,
schema-versioned, scoped to workspace / profile / preferences,
exportable.

In Nomi today, the memory subsystem is a thin homegrown SQLite store
with the same scoping. Integration with mnemos as an embedded library
is on the roadmap — see the mnemos integration assessment in
`.roady/spec.yaml` for the path.

If your agent needs to remember things across runs and you want the
same SQL surface you use everywhere else, mnemos is the right shape.

### [`scout`](https://github.com/felixgeelhaar/scout) — sight

Browser automation built for agents — observable DOM, semantic
selectors, deterministic end-to-end. Used by the Nomi user-journey
test runner today; the Browser plugin will adopt it once that
connector ships.

If your agent has to drive a web UI and you've been wrestling
Playwright into agent-shaped tasks, scout is opinionated for the
agent case from the first commit.

### [`roady`](https://github.com/felixgeelhaar/roady) — plan

Spec-driven planning and task tracking with a hash-chained audit log.
Every Nomi feature change passes through a roady spec before code
lands; see [`.roady/`](../.roady/) for the live spec, plan, and state.

If you're tired of pretending issue trackers are planning systems —
or if the plan-and-the-code don't agree by Sprint 3 — roady is the
fix.

## External runtime dependencies

The application layer also leans on:

[Tauri](https://tauri.app),
[Gin](https://github.com/gin-gonic/gin),
[modernc.org/sqlite](https://gitlab.com/cznic/sqlite),
[wazero](https://wazero.io),
[Ollama](https://ollama.com),
[shadcn/ui](https://ui.shadcn.com),
[Radix UI](https://www.radix-ui.com),
[TanStack Query](https://tanstack.com/query/latest).

## ADRs

Architecture decision records live under [`docs/adr/`](adr/) and cover
the load-bearing choices: plugin architecture, permission engine
shape, state-machine model, event bus design.
