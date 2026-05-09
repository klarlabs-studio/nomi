# Coding Agent — local Claude-Code-style loop

Read your repo, see a unified-diff preview before any write, run tests after
you approve. The whole loop runs against a local Ollama model so the code
stays on your machine.

## What you get

- A **Coding Agent** assistant wired with `filesystem.read`, `filesystem.patch`,
  `filesystem.write`, and `command.exec` (the last two gated by **confirm**).
- A **sample-repo/** Go module you can edit end-to-end without touching real code.
- A workflow the agent has been trained to follow:
  1. `filesystem.context` to orient.
  2. `filesystem.read` the files it intends to change.
  3. Propose a `filesystem.patch` step with a unified diff.
  4. Run `go test ./...` after you approve.

## 90-second walkthrough

Pull a coding model that does well at structured edits:

```bash
ollama pull qwen2.5-coder:14b
```

Apply the recipe:

```bash
nomi seed examples/coding-agent/seed.yaml
```

Open the desktop app, pick the **Coding Agent** assistant, and ask:

> Add a `json:"name"` struct tag to the `Name` field of the `User` struct in
> `models.go`. Then run `go test ./...`.

The plan-review surface will show three steps:

1. **Read models.go** — `filesystem.read` (no approval needed).
2. **Patch User struct** — `filesystem.patch` with a unified diff preview.
3. **Run go test ./...** — `command.exec` (gated to `go`/`git`/`npm`/`make`).

Approve the plan, then approve each step's capability prompt as it fires.
The diff applies cleanly; the test run prints `PASS`. That's the wedge: the
model proposed the edit, you saw it before it touched disk, and you ran the
suite to confirm it works.

## Why this beats `filesystem.write`

`filesystem.write` requires the planner to emit the whole file body — slow,
token-heavy, and error-prone for big files. `filesystem.patch` ships only
the changed lines, the runtime feeds them to `git apply`, and the UI shows
exactly what's changing inside a `+/−` block. Approval rules don't change:
`filesystem.patch` shares the `filesystem.write` capability so a single
"confirm writes" rule covers both.

## Tweaking the recipe

- **Different model**: edit `provider.default_model` in `seed.yaml`.
- **Different repo**: change `assistants[0].workspace` to your project path.
- **Tighter command allowlist**: edit the `coding-agent` permission policy
  (`templates/built-in.json`) and reseed; `allowed_binaries` defaults to
  `[go, git, npm, make]`.
