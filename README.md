<h1 align="center">Nomi</h1>

<p align="center">
  <strong>Your personal AI runtime.</strong><br />
  Local-first daemon for plans, tools, memory, and any model — designed
  for inspection before execution. Multi-step plans you approve,
  capability-gated tools, and persistent memory via Mnemos. Coding is
  the first flagship workflow; the runtime underneath is general.
</p>

<p align="center">
  <a href="https://github.com/felixgeelhaar/nomi/releases/latest"><img src="https://img.shields.io/github/v/release/felixgeelhaar/nomi?include_prereleases&color=blue" alt="release"></a>
  <a href="https://github.com/felixgeelhaar/nomi/actions/workflows/release.yml"><img src="https://img.shields.io/github/actions/workflow/status/felixgeelhaar/nomi/release.yml?branch=main" alt="build"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/felixgeelhaar/nomi" alt="license"></a>
  <img src="https://img.shields.io/badge/local--first-yes-green" alt="local-first">
  <a href="https://github.com/felixgeelhaar/nomi/stargazers"><img src="https://img.shields.io/github/stars/felixgeelhaar/nomi?style=social" alt="stars"></a>
</p>

<p align="center">
  <a href="#compared-to">Compared to</a> •
  <a href="#install">Install</a> •
  <a href="#quickstart">Quickstart</a> •
  <a href="#features">Features</a> •
  <a href="#powered-by">Stack</a> •
  <a href="#roadmap">Roadmap</a> •
  <a href="#contributing">Contributing</a>
</p>

<p align="center">
  <video src="https://github.com/felixgeelhaar/nomi/raw/main/docs/media/hero.mp4"
         poster="docs/media/hero-poster.jpg"
         width="900"
         controls
         muted
         playsinline>
    <a href="https://github.com/felixgeelhaar/nomi/blob/main/docs/media/hero.mp4">
      <img src="docs/media/hero-poster.jpg" alt="Plan → Approve → Run — 90s demo" width="900" />
    </a>
  </video>
</p>
<p align="center"><sub><strong>90s demo.</strong> Type a goal · review the plan · approve · run. Recorded against a local Ollama.</sub></p>

---

## Why Nomi

Most agents are stateless execution engines. They read your context,
run tools, then forget everything when the session ends. The reasoning,
the decisions, the operational knowledge — gone. Nomi treats that as a
category mistake. The product is a **runtime**: a daemon that holds
durable state, gates tools through an explicit capability engine, and
remembers across runs via [Mnemos](https://github.com/felixgeelhaar/mnemos).
Coding is the first high-frequency workflow — the same primitives carry
the next ones.

- **Local-first by default — self-hosted by choice.** On a laptop the
  data, conversations, and secrets stay on your machine (SQLite, OS
  keyring, no telemetry, no account). On a homelab box or a cloud VM
  the same `nomid` daemon runs headless behind your reverse proxy —
  see [`docs/headless.md`](docs/headless.md).
- **Plan review before execution.** Every multi-step task is laid out
  in full before any tool runs. You see the plan; you approve the plan.
  Inspectable, editable, replayable.
- **Capability engine, not a coding-safety knob.** `filesystem.write`,
  `command.exec`, `network.outgoing`, custom capabilities from plugins
  — every tool is bound by an explicit permission rule. Allow, confirm,
  or deny. Per-assistant. Same primitive whether you're writing code,
  triaging email, or driving a browser.
- **Mnemos — the cognitive layer.** Workspace-scoped, queryable,
  exportable memory persisted to SQLite. A real database, not a vector
  blob. The agent accumulates context across runs so the next task
  starts with the last one's lineage.
- **Bring any LLM.** Ollama for free + private. Anthropic / OpenAI when
  you want frontier models. LM Studio, vLLM, Together — anything that
  speaks the OpenAI or Anthropic wire format. Per-assistant overrides
  ship out of the box.
- **Daemon-not-IDE-plugin.** `nomid` is a long-running runtime, not a
  sidecar. Desktop UI, CLI, headless server — all clients of the same
  daemon. Real plugins (Telegram today, WASM marketplace next), real
  isolation, no bypass paths.

## Compared to

Nomi is a **runtime + interaction model**, not another coding-agent.
The coding wedge today is **Claude Code with local Ollama** — but the
same daemon, capability engine, and Mnemos memory layer underneath
every other workflow you'll run next.

| Alternative | What's different about Nomi |
|---|---|
| **Claude Code / Cursor agents / Cline** | Same goal-driven coding flow (read repo, plan changes, write files, run commands), but every step is laid out as an approveable plan first, every tool call is gated by an explicit capability, and every event is persisted to a hash-chained audit log. Point it at Ollama and your repo never leaves your laptop. |
| **Goose / OpenInterpreter / Aider** | Same local-first stance, but with a real state machine (`Run → Plan → Step`), a real permission engine, real multi-step plans the user can edit, and a desktop UI built around the approval moment instead of around the chat box. |
| **OpenClaw / Hermes Agent** | Same personal-AI-with-memory framing, broader connector list shipping today. Both act the moment the model decides; Nomi inserts a `plan_review` state before any tool runs and gates every tool through an explicit capability rule. Mnemos is memory you can **read**, edit, and export — Hermes's "self-improving" memory is opaque by design. Nomi trades connector breadth today for inspectability everywhere. |
| **NanoClaw / container-isolation products** | NanoClaw isolates agents with Docker / micro-VM / Apple Container — the wall is around the process. Nomi gates at the capability layer — the agent never reaches the tool it shouldn't because the rule blocked the call. The two compose: you could run `nomid` inside a NanoClaw container and stack both defenses. NanoClaw is Anthropic-SDK-biased; Nomi is provider-agnostic. |
| **Pi (Inflection AI)** | Category boundary. Pi is a thoughtful conversational companion — cloud-only, closed, no tool execution. Use Pi when you want an AI to talk to. Use Nomi when you want an AI that takes action you approved. |
| **LangChain / AutoGPT / CrewAI** | Those are kits — you assemble the agent. Nomi is the finished product: a working state machine, a permission engine, a memory subsystem, a Tauri shell, all wired up. |
| **Bespoke agent stacks** | Stop reinventing scaffolding. The runtime, the audit trail, the approval workflow, and the plugin model all ship today. Bring your assistants and your prompts. |

Full feature-by-feature breakdown:
[`docs/comparison.md`](docs/comparison.md).

## Install

**Desktop app (Tauri shell + bundled `nomid` daemon):**

| Channel | Command |
|---|---|
| **Homebrew Cask (macOS)** | `brew install --cask felixgeelhaar/tap/nomi` |
| **DMG / MSI / AppImage / DEB** | [Releases page](https://github.com/felixgeelhaar/nomi/releases/latest) |

**CLI (`nomi` — drives a local or remote daemon over REST):**

| Channel | Command |
|---|---|
| **Homebrew (macOS / Linux)** | `brew install felixgeelhaar/tap/nomi` |
| **Direct download (Windows)** | [Releases page](https://github.com/felixgeelhaar/nomi/releases/latest) — `nomi-*-windows-amd64.zip` |
| **`go install`** | `go install github.com/felixgeelhaar/nomi/cmd/nomi@latest` |

**Headless daemon (`nomid`):**

| Channel | Command |
|---|---|
| **Docker** | `docker run -p 8080:8080 -v nomi-data:/data ghcr.io/felixgeelhaar/nomi` |
| **`go install`** | `go install github.com/felixgeelhaar/nomi/cmd/nomid@latest` |

The desktop bundle ships the `nomid` runtime as a Tauri sidecar — one
installer, both binaries. **Docker / `go install` give you just the
daemon** — drop it on a homelab box, a VPS, a Kubernetes pod, anywhere
that runs Linux. Configure via a YAML seed manifest at first boot, or
drive the REST API directly. Full guide:
[`docs/headless.md`](docs/headless.md).

For headless interaction without the desktop UI, the **`nomi` CLI**
talks to the daemon over the same REST surface:

```bash
nomi status                              # health + version + active default LLM
nomi run "summarize notes.md"            # submit, drive, print output
nomi list runs                           # most recent runs as a table
nomi list approvals                      # pending approval cards
nomi tail                                # follow the SSE event stream live
nomi seed examples/seed.yaml             # apply a YAML manifest
nomi export -o nomi.yaml                 # snapshot full config (commit to git)
nomi import nomi.yaml                    # reproduce that config on another box

# Drive a remote daemon over SSH-fetched token
NOMI_TOKEN=$(ssh server 'docker exec nomi cat /data/auth.token') \
    nomi --url=https://nomi.example.com run "what changed today?"
```

The CLI auto-resolves URL + token from `$NOMI_DATA_DIR/api.endpoint`
and `$NOMI_DATA_DIR/auth.token` when it runs on the same host as the
daemon.

```yaml
# examples/seed.yaml — mounted at /data/seed.yaml or pointed at via NOMI_SEED.
# Idempotent: edit + restart picks up the diff.
provider:
  name: Ollama
  type: local
  endpoint: http://host.docker.internal:11434
  model_ids: [qwen2.5:14b]
assistants:
  - template_id: research-assistant
    workspace: /data/workspace
settings:
  safety_profile: balanced
  onboarding_complete: true
```

## Who this is for

You're a developer who works on code your security team doesn't want
in someone else's cloud. You'd use Claude Code or Cursor agents if the
model ran on your machine. Ollama on a 14B model is enough for the
edits you actually make; you just want a real agent surface — plans,
approvals, audit log — wrapped around it.

If that's not you, Nomi probably still runs your inbox, your research
folder, or a Telegram bot — see [`examples/`](examples/) for other
recipes — but the wedge it's built around is the dev who won't paste
source into a remote LLM.

## Quickstart

```bash
# 1. Local LLM (or skip and use Anthropic / OpenAI from the wizard)
#    qwen2.5-coder is the recommended model for the Coding Agent
#    flagship recipe; swap in qwen2.5 for general-purpose chat.
brew install ollama
ollama serve &
ollama pull qwen2.5-coder:7b

# 2. Install Nomi
brew install --cask felixgeelhaar/tap/nomi

# 3. Open Nomi → wizard sets provider + assistant + workspace in <60s
# 4. Type a goal in chat → review the plan → approve → watch it run
```

### Your first 5 minutes

```text
$ nomi run "Add a JSON tag to the User struct in models.go"

✓ run.created            id=r_8a2  goal="Add a JSON tag to the User struct in models.go"
✓ plan.proposed          steps=3
   1. filesystem.read    path=models.go
   2. filesystem.patch   path=models.go      ← needs your approval
   3. command.exec       cmd="go test ./..." ← needs your approval

[plan-review] Approve? [y/n/edit]: y
✓ approval.granted       step=2  by=user
✓ step.completed         tool=filesystem.patch  diff=+1 -1
✓ approval.granted       step=3  by=user
✓ step.completed         tool=command.exec      exit=0
✓ run.completed          duration=11s
```

Three places to land next:

1. **Try the flagship recipe** — [`examples/coding-agent/`](examples/coding-agent/) walks the read → unified-diff preview → approve → `go test` loop above against a tiny sample repo. [`examples/code-reviewer/`](examples/code-reviewer/) is the read-only sibling.
2. **Talk to other people** — [GitHub Discussions](https://github.com/felixgeelhaar/nomi/discussions) for questions, [issues](https://github.com/felixgeelhaar/nomi/issues) for bugs.
3. **Watch where v0.2 lands** — the v0.2 flagship is real LLM-backed multi-step plans + Anthropic streaming; subscribe via [GitHub Releases](https://github.com/felixgeelhaar/nomi/releases) → Watch → Custom → Releases.

## Features

### Plan, review, execute

<p align="center"><img src="docs/images/02-plan-review.png" width="900" alt="A multi-step plan with each tool call laid out before execution starts"></p>

Every task becomes a plan with explicit tool calls. Edit the plan,
branch from any step, or reject it entirely.

<details>
<summary><strong>More features (click to expand)</strong></summary>

### Approvals as a first-class flow
<p align="center"><img src="docs/images/09-approvals.png" width="900" alt="Approval card"></p>

Confirm-mode capabilities pause the run and surface a plain-language
card. "Remember this choice for 24 hours" if the same kind of action
keeps coming up.

### Memory you can see and edit
<p align="center"><img src="docs/images/04-memory.png" width="900" alt="Memory inspector"></p>

Workspace, profile, and preferences scopes. The agent saves what it
learns; you keep control of what's there.

### Plugins, not integrations
<p align="center"><img src="docs/images/06-plugins.png" width="900" alt="Plugins tab"></p>

Each plugin declares its capabilities and runs through the same
permission engine as the core tools. Connect what you need; nothing
else loads.

### Bring your own model
<p align="center"><img src="docs/images/07-providers.png" width="900" alt="AI providers tab"></p>

Ollama, Anthropic, OpenAI, vLLM, LM Studio, Together, Groq — anything
on the OpenAI or Anthropic wire format. Set a global default; override
per assistant.

### Safety profiles
<p align="center"><img src="docs/images/08-safety.png" width="900" alt="Safety profile picker"></p>

Three profiles for the default permission stance on new assistants.
Balanced is recommended; Cautious confirms everything; Fast trades
safety for iteration speed.

### Audit log
<p align="center"><img src="docs/images/05-events.png" width="900" alt="Event log"></p>

Every state transition emits an event. Hash-chained, exportable,
queryable by run id. The runtime is observable without any external
integration.

### Assistants
<p align="center"><img src="docs/images/03-assistants.png" width="900" alt="Assistants tab"></p>

Each assistant carries its own persona, capability ceiling, permission
policy, folder context, model override, and bound plugin connections.

</details>

## Powered by

Nomi is built from composable cognitive-stack libraries you can use
independently. Each is Apache-2.0, documented, releasable on its own:

- **[`statekit`](https://github.com/felixgeelhaar/statekit)** — finite
  state machines for Go (powers every `Run` / `Plan` / `Step`).
- **[`mnemos`](https://github.com/felixgeelhaar/mnemos)** —
  evidence-backed local-first knowledge engine *(integration in flight)*.
- **[`scout`](https://github.com/felixgeelhaar/scout)** — browser
  automation built for agents.
- **[`roady`](https://github.com/felixgeelhaar/roady)** — spec-driven
  planning + task tracking with hash-chained audit log.

Full architecture and the case for each library:
[`docs/architecture.md`](docs/architecture.md).

## Architecture

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
│  │  Tool registry  ·  LLM resolver  ·  Memory  →  mnemos    │
│  ├────────────────────────────────────────────────────┤     │
│  │  Plugin registry  ·  Browser  →  scout             │     │
│  │  WASM host (wazero)                                │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  Event bus  →  SSE stream  +  hash-chained audit   │     │
│  ├────────────────────────────────────────────────────┤     │
│  │  SQLite (WAL) · embedded migrations · OS keyring   │     │
│  └────────────────────────────────────────────────────┘     │
└──────────────────────────┬──────────────────────────────────┘
                           │  OpenAI-compat / Anthropic / Ollama
                           ▼
                    LLM provider(s)
```

ADRs under [`docs/adr/`](docs/adr/) cover the big decisions
(plugin architecture, permission engine, state machine).

## Development

```bash
# Backend (Go)
make build          # builds bin/nomid
make test           # go test -race ./...
make sidecar        # builds bin/nomid-<host-target-triple> for Tauri bundling
make migrate-up     # runs embedded migrations against ~/.config/Nomi/nomi.db

# Desktop app (Tauri + Vite)
make app-dev        # dev server at :5173, daemon spawned automatically
make app-build      # produces a signed DMG / MSI / AppImage / DEB

# End-to-end user-journey tests (real Ollama required)
test/journeys/run.sh    # 22 journeys; pass j1 j7 j20 to scope
```

The full developer surface — including the user-journey definitions
every release ships against — is in
[`docs/user-journeys.md`](docs/user-journeys.md).

## Roadmap

**v0.2 — "Plans That Plan."** Shipped: LLM-generated multi-step
plans, capability-routed tools, streaming tokens, JSON mode + few-shot
exemplars, self-repair retry loop, prompt-injection trust-boundary
tags, replan-on-failure (planner sees prior step output + failure and
proposes a corrective plan, bounded by MaxReplansPerRun), the
[`coding-agent` flagship recipe](examples/coding-agent/) with
unified-diff previews, hash-chained audit log, and Prometheus
`/metrics` for plan / step / replan attribution per provider.

Backlog (post-v0.2, in priority order — not committed):

- NomiHub plugin marketplace (signed WASM, install/update flow).
- `nomi tui` — bubbletea TUI for SSH-only workflows.
- Cross-device sync (opt-in, end-to-end-encrypted).
- Vision backend for the media plugin (LLaVA via Ollama).

Live spec, plan, and task state in [`.roady/`](.roady/); ideas and bug
reports on the [issues page](https://github.com/felixgeelhaar/nomi/issues).

## Contributing

Pull requests welcome. Read the [`docs/adr/`](docs/adr/) entries before
changing a load-bearing subsystem (permission engine, plugin
architecture, runtime state machine), then open an issue to discuss.
Smaller fixes — typos, doc edits, plugin polish — can land straight as
a PR.

Look for [`good first issue`](https://github.com/felixgeelhaar/nomi/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
labels on the issues board.

The project follows the [Contributor Covenant Code of
Conduct](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).

## License

Apache-2.0. See [`LICENSE`](LICENSE).
