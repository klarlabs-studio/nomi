# Examples

Drop-in recipes that show off a single Nomi behavior end-to-end. Each
recipe is a folder with a README and a `seed.yaml` manifest you apply
via the CLI:

```bash
nomi seed examples/<recipe>/seed.yaml
```

## Coding-agent recipes (the wedge)

| Recipe | What it does | Plugin |
|---|---|---|
| [`coding-agent`](coding-agent/) | **Flagship.** Read repo, propose unified-diff edits via `filesystem.patch`, see diff preview before approve, run `go test ./...` after. | none |
| [`code-reviewer`](code-reviewer/) | Point an assistant at one repo. Reads files, plans changes, asks before writing (no patch). | none |

## Other recipes

Nomi also runs personal-AI workflows. Sharing the same runtime, but
not the wedge launch focus:

- [`research-assistant`](research-assistant/) — read a folder of PDFs / markdown, summarize, draft synthesized notes.
- [`inbox-triage`](inbox-triage/) — classify and draft replies for messages forwarded to a Telegram bot.

## Building your own

The full seed manifest schema lives in
[`examples/seed.yaml`](seed.yaml) (the canonical reference). Templates
available out of the box: `coding-agent`, `code-reviewer`,
`research-assistant`, `writing-partner`, `learning-tutor`,
`inbox-triage`, `github-pr-reviewer`, `custom`.

Each assistant gets its own permission policy, model override, and
folder context — see [`docs/architecture.md`](../docs/architecture.md)
for the underlying state machine and capability model.
