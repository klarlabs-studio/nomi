# Recipe: Code Reviewer

A local-first code review agent that reads your repo, plans changes,
and asks before it writes anything. Plugged at a single repo via the
folder-context binding. No code leaves your machine unless you point
the provider at a remote LLM.

## What it does

- Reads files in the workspace folder (`filesystem.read`,
  `filesystem.list`, `filesystem.context`).
- Proposes patches as a plan-review card before any write
  (`filesystem.write` is gated `confirm`).
- Refuses anything outside the workspace folder via the permission
  engine.
- Persists every plan, approval, and event to SQLite.

## Apply

```bash
# 1. Edit seed.yaml — point `workspace` at your repo, set the LLM
#    provider (Ollama for fully offline, Anthropic / OpenAI for
#    frontier models).
# 2. Apply via the CLI:
nomi seed examples/code-reviewer/seed.yaml

# 3. Open the desktop app, pick "Code Reviewer", give it a goal:
#    "Review src/auth/* for input-validation issues"
```

## Try it

Goals that show off the wedge:

- **"Find places we forget to handle errors in `internal/runtime/`."**
  Watch the planner read files, flag spots, propose a patch, ask
  before writing.
- **"Update the README to document the new flag I added."**
  Plan: read README, draft replacement, await approval, write.
- **"Run the tests for the package I just touched."**
  `command.exec` is approval-gated by default — you'll see the
  exact command before it runs.

## Files

- [`seed.yaml`](seed.yaml) — minimal manifest: one provider, one
  Code Reviewer assistant, workspace pointer.
