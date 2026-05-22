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

## Personal AI assistant category

A different category of competitor: products positioned as "your
personal AI agent" rather than coding-first assistants. OpenClaw,
NanoClaw, and Hermes all reach into messaging apps (WhatsApp, Telegram,
Slack, Discord, …), keep durable memory, and do things on your machine.
Pi sits at the boundary — same "personal AI" framing, but conversational
companion only, no workflow execution.

| Capability | **Nomi** | OpenClaw | NanoClaw | Hermes Agent | Pi (Inflection) |
|---|---|---|---|---|---|
| Local-first daemon | ✅ `nomid` + SQLite | ✅ Local + BYO API key | ✅ Local in Docker | ✅ Self-hosted | ✕ Cloud only |
| Plan review **before** execution | ✅ `plan_review` state | ✕ Acts immediately | ✕ Acts immediately | ✕ Acts immediately | n/a (no tools) |
| Capability-gated tools | ✅ Per-assistant `allow / confirm / deny` | ✕ Broad system access | ◐ Container isolation (different layer) | ◐ Per-action confirm | n/a |
| Persistent memory | ✅ Mnemos — workspace-scoped, queryable, exportable | ◐ Conversation memory | ◐ Per-user memory | ✅ Self-improving memory (opaque) | ✅ Cloud memory (opaque) |
| Audit trail | ✅ Hash-chained event log, `/audit/verify` | ✕ Session log | ◐ Container logs | ◐ Session log | ✕ |
| Plugin isolation | ✅ WASM + ed25519 signed | ✕ Plain JS / npm extensions | ✅ Docker / micro-VM / Apple Container | ◐ Tool registry | n/a |
| Connector breadth (shipped) | Telegram (roadmap: 10+) | 20+ messaging apps + email | 13+ (WhatsApp, Telegram, Slack, Teams, Gmail, iMessage, Matrix, GitHub, Linear, …) | Telegram-primary + others | n/a |
| Model strategy | ✅ BYO any (Ollama, Anthropic, OpenAI, OpenAI-compatible) | ✅ BYO API key | ◐ Anthropic Agents SDK bias | ✅ 300+ via OpenRouter + direct | ✕ Inflection 2.5 only |
| License | Apache-2.0 | Apache-2.0 (non-profit stewardship) | MIT | Open source | Proprietary |
| Threat model | Agent does the wrong thing → gated by capability engine + plan review | Agent has broad reach by design → user trusts the agent | Agent escapes execution boundary → contained by Docker / micro-VM | Agent does the wrong thing → confirm-per-action | n/a |
| GitHub stars (May 2026) | low (early) | 60K+ in 72hrs | ~29K | 60K+ in 2 months | n/a (closed) |

Legend: ✅ shipped · ◐ partial / different shape · ✕ not present · n/a not applicable.

### Nomi vs **OpenClaw**

OpenClaw is the breadth-of-integrations comparison and the most likely
"this is what people in this category mean." Differences:

- **Plan review before execution.** OpenClaw acts the moment the model
  decides. Nomi inserts a `plan_review` state before any tool runs —
  the human sees, edits, and approves the plan first.
- **Capability engine.** OpenClaw has broad system access by design;
  that's the feature. Security researchers found RCE and malicious
  third-party extensions because the surface is wide. Nomi's
  capability rules (`filesystem.write`, `command.exec`,
  `network.outgoing`, …) are explicit per-assistant `allow / confirm /
  deny` — the rule can't be talked around with prompt-engineering.
- **Plugin posture.** OpenClaw's extension model is JS / npm. Nomi
  ships WASM (wazero) + ed25519 signature verification — narrower
  capability surface, signed supply chain.
- **Connector breadth today.** OpenClaw wins outright — 20+ messaging
  apps shipping. Nomi has Telegram today; the rest is on the roadmap.

When OpenClaw wins: you want maximum connector coverage now and you
trust the agent broadly. When Nomi wins: you want to inspect a plan
before it runs and you want the broad reach to come with hard
capability rules around it.

### Nomi vs **NanoClaw**

NanoClaw is closer in spirit than OpenClaw — both products think
seriously about agent safety. Different layers of the threat model:

- **Different blast radius defense.** NanoClaw isolates by **container**
  (Docker, optional micro-VM, Apple Container on macOS): if the agent
  goes off, the container is the wall. Nomi gates by **capability**:
  the agent never reaches the tool it shouldn't because the rule
  blocked the call. The two compose — you could run `nomid` inside a
  NanoClaw container and stack both layers.
- **Model strategy.** NanoClaw runs on Anthropic's Agents SDK; Anthropic
  bias is real. Nomi is provider-agnostic — Ollama, OpenAI, Anthropic,
  any OpenAI-compatible endpoint, per-assistant override.
- **Plan review.** NanoClaw doesn't surface a plan-before-execution
  state. Nomi does.
- **Connectors today.** NanoClaw ships ~13 messaging connectors out of
  the box. Nomi ships Telegram.

When NanoClaw wins: you need container-grade isolation today, you're
comfortable on Anthropic models, you want the connector breadth now.
When Nomi wins: you want plan-review + per-tool capability rules, BYO
model, and a state machine you can inspect.

### Nomi vs **Hermes Agent** (Nous Research)

Hermes is the closest competitor on the **memory-as-product** axis —
both Nomi (via Mnemos) and Hermes treat persistent memory as the
defining feature. Differences in what "memory" means:

- **Inspectable vs self-improving.** Hermes's memory is opaque —
  "self-improving," learned from interactions, model-implicit. Mnemos
  is the opposite: workspace-scoped SQLite rows, queryable, editable,
  exportable as JSONL. You can read what your agent remembers. You can
  delete a single entry. You can ship the database to a new machine.
- **Inspection before execution.** Hermes acts; Nomi reviews then acts.
  Same axis as OpenClaw.
- **Form factor.** Hermes optimizes for "AI in your pocket via
  Telegram, anywhere." Nomi optimizes for "AI as a desktop and
  headless runtime, plan-reviewable from the UI you're using." Both
  have Telegram; the primary surface differs.
- **Model choice.** Both BYO model. Hermes via OpenRouter (300+
  models); Nomi via direct provider profiles. Slightly different
  ergonomics, same outcome.

When Hermes wins: you want maximum mobile-first memory with the model
learning your style implicitly, and you trust the agent to act
without an approval gate. When Nomi wins: you want memory you can
**read** and an agent that asks before acting.

### Nomi vs **Pi** (Inflection AI) — category boundary

Pi is the "category boundary" entry: same "personal AI" framing, but
different shape entirely.

- **What Pi is.** Conversational companion, EQ-focused. Voice + chat.
  Free, cloud, closed-source, Inflection 2.5 only. Microsoft acquihired
  the founding team in 2024; the product remains live but development
  momentum is reduced.
- **What Pi isn't.** A workflow runtime. Pi doesn't read your files,
  doesn't run shell commands, doesn't write to your filesystem, doesn't
  send a message on your behalf. There's nothing to approve because
  there's nothing being executed.
- **Why it's in this table.** Because users searching for "personal AI"
  land on both. If you want a thoughtful conversation, Pi is the
  better tool. If you want an agent that does things — with the
  guardrails to do them safely — that's Nomi.

When Pi wins: you want a kind, conversational AI for reflection and
advice. When Nomi wins: you want the agent to take action, but only
the actions you saw and approved.

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
