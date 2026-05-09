# Comparison

How Nomi differs from other agent platforms. Facts only — every column
maps to behavior shipped today, not roadmap claims.

> **Disclosure:** rows describe Nomi as of v0.1.x and competitor projects
> as of early 2026. Competitor capabilities evolve quickly; if a row is
> stale, please open an issue.

## Feature matrix

| Capability | **Nomi** | Goose | Cline | OpenInterpreter | Claude Code | AutoGPT | CrewAI |
|---|---|---|---|---|---|---|---|
| Local-first | ✅ Daemon + SQLite on your box | ✅ | ◐ Runs in editor; data in workspace | ✅ | ◐ CLI local; model is Claude only | ◐ Local exec, remote LLM by default | ✕ Library, no daemon |
| Plan review **before** execution | ✅ `plan_review` state, edit + approve before any tool call | ✕ Per-tool approval mid-run | ✅ Plan/Act mode | ✕ Confirm-per-block | ✅ Plan mode | ✕ Self-driven | ✕ |
| Capability-gated tools | ✅ Per-assistant `allow / confirm / deny` rules | ◐ Per-tool prompts | ◐ Per-action prompts | ◐ Per-block confirm | ◐ Allowlist | ✕ | ✕ |
| Persisted event log | ✅ Full event stream in SQLite, queryable + streamable + hash-chained (`GET /audit/verify` walks the chain) | ◐ Session log | ◐ Editor history | ◐ Console log | ◐ Session transcripts | ✕ | ✕ |
| Approval workflow (out-of-band) | ✅ Approval cards in desktop UI; resolvable later | ✕ Inline only | ✕ Inline only | ✕ Inline only | ✕ Inline only | ✕ | ✕ |
| BYO LLM | ✅ Anthropic, OpenAI, Ollama; per-assistant override | ✅ | ✅ | ✅ | ✕ Anthropic only | ✅ | ✅ |
| Desktop UI | ✅ Tauri shell (macOS / Linux / Windows) | ✅ Goose Desktop | ◐ VSCode extension | ◐ Terminal + 01 device | ✕ CLI | ◐ Web UI | ✕ |
| Plugin sandbox | ✅ WASM (wazero) + ed25519 signing | ◐ MCP servers | ◐ MCP servers | ✕ | ◐ MCP servers | ◐ Python plugins | ◐ Python tools |
| License | Apache-2.0 | Apache-2.0 | Apache-2.0 | AGPL-3.0 | Proprietary | MIT | MIT |

Legend: ✅ shipped · ◐ partial / different shape · ✕ not present.

## Detail

### Nomi vs **Claude Code**

Same coding-agent loop (read repo, plan, edit files, run commands). Two
differences worth your attention:

- **Where the model runs.** Claude Code is bound to Anthropic's hosted
  models. Nomi takes a provider profile: paste an Anthropic key, an
  OpenAI key, or point at `localhost:11434` for Ollama. The repo can
  stay on your laptop.
- **How approvals work.** Claude Code prompts in the terminal at the
  moment a tool runs. Nomi's `plan_review` state lets you see (and
  edit) the whole plan before any step executes, and approval cards
  surface in a desktop UI that you can return to later — useful when
  the plan was kicked off from another device or a non-interactive
  context.

When Claude Code wins: you live in the terminal, you're already paying
for Claude, you don't want a separate process. When Nomi wins: you
want to swap the model freely and you want a UI surface for approvals.

### Nomi vs **Cline**

Cline ships Plan/Act mode inside VSCode and is the closest competitor
on the plan-review axis. Two structural differences:

- **Where the agent lives.** Cline is a VSCode extension; Nomi is a
  daemon (`nomid`) plus a Tauri shell. The daemon also runs headless on
  a homelab box, a VPS, or a Kubernetes pod, and the same approvals
  surface in the desktop client over REST + SSE.
- **What the audit trail looks like.** Cline keeps a session/editor
  history. Nomi persists every event (run, plan, step, approval, tool
  call) to SQLite, queryable via `/events` and streamable via SSE.

When Cline wins: you live in VSCode and want zero context switch. When
Nomi wins: you want the agent to run on a machine you don't have an
editor open on.

### Nomi vs **Goose**

Goose ships a desktop client and a strong MCP story. Differences:

- **Plan review.** Goose approves per-tool inline at execution time.
  Nomi commits to an explicit `plan_review` state in the run state
  machine, so you can edit the plan, branch from it, and only then
  approve.
- **Plugin sandbox.** Goose extends through MCP servers (subprocesses).
  Nomi runs plugins as WASM via wazero with ed25519 signature
  verification — narrower capability surface, smaller attack surface.

When Goose wins: you want MCP-first interop with a wider plugin
ecosystem. When Nomi wins: you want signed-WASM plugin isolation and
plan-review-as-a-state, not as a habit.

### Nomi vs **OpenInterpreter**

OpenInterpreter executes code locally with per-block confirmation and
is the spiritual ancestor of "agent on your machine". Differences:

- **Run model.** OpenInterpreter generates code, asks before each
  block, and runs. There's no plan-review surface and no formal state
  machine — execution is one block at a time.
- **Capabilities.** OpenInterpreter's confirmation is binary
  (run / don't run). Nomi's capability rules are per-assistant and
  per-tool — `filesystem.write` can be `confirm`, `command.exec` can
  be `deny`, both within the same agent.
- **License.** OpenInterpreter is AGPL-3.0; Nomi is Apache-2.0. If
  you embed the code in a product, that matters.

When OpenInterpreter wins: terminal-first, fast iteration, you don't
need the state machine. When Nomi wins: you want explicit
plan-then-execute and per-capability rules.

### Nomi vs **AutoGPT**

AutoGPT is the canonical "self-driving agent kit". Differences:

- **Workflow.** AutoGPT self-prompts toward a goal. Nomi is
  user-in-the-loop: every plan goes through `plan_review`, every
  gated tool through an approval card.
- **Form factor.** AutoGPT is a web UI + Python runtime; Nomi is a Go
  daemon + Tauri desktop client.

When AutoGPT wins: you want to delegate fully and check the result.
When Nomi wins: you want to see and steer the plan.

### Nomi vs **CrewAI**

CrewAI is a Python library for multi-agent orchestration. Different
shape:

- **It's a kit.** CrewAI is `pip install` and you write the
  orchestration code. Nomi is a finished application — daemon, REST
  API, desktop UI, plugin manager all wired up.
- **Approval and audit.** CrewAI doesn't ship a built-in
  user-approval workflow or persisted event log; you'd build that on
  top.

When CrewAI wins: you want a programming model for multi-agent
flows and you don't need an end-user surface. When Nomi wins: you
want a runnable agent product, not a framework to assemble one.

## What Nomi does **not** do (yet)

To keep the comparisons honest:

- **Telegram is the only shipped connector** today. Email, Slack,
  Discord, Calendar, Obsidian and the rest of the README "Plugins"
  list are roadmap items.
- **No multi-tenant / team mode.** Nomi runs as a single-user local
  daemon. Cross-device sync (E2E-encrypted) is on the post-V1
  roadmap, not shipped.
- **Multi-tool planner is shipped** with replan-on-failure, prompt-injection
  trust-boundary tags, JSON mode, few-shot exemplars, and a self-repair
  retry loop. The flagship recipe lives at [`examples/coding-agent/`](../examples/coding-agent/)
  — read repo → unified-diff preview → approve → run tests, all
  against your local Ollama model.
- **No mobile client.** REST API is reachable from anything; there's
  no first-party iOS / Android app.
- **No hosted offering.** Nomi is software you run, not a service.

If any of these gaps are blockers for your use case, one of the tools
above is probably the better fit today.
