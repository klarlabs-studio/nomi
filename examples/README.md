# Examples

Drop-in recipes that show off a single Nomi behavior end-to-end. Each
recipe is a folder with a README and a `seed.yaml` manifest you apply
via the CLI:

```bash
nomi seed examples/<recipe>/seed.yaml
```

## Recipes

| Recipe | Wedge | Plugin |
|---|---|---|
| [`code-reviewer`](code-reviewer/) | Local-first code review on a single repo. Plan → approve → patch. | none |
| [`research-assistant`](research-assistant/) | Read a folder of PDFs / markdown, summarize, draft synthesized notes. | none |
| [`inbox-triage`](inbox-triage/) | Classify and draft replies for messages forwarded to a Telegram bot. | Telegram |

## Building your own

The full seed manifest schema lives in
[`examples/seed.yaml`](seed.yaml) (the canonical reference). Templates
available out of the box: `code-reviewer`, `research-assistant`,
`writing-partner`, `learning-tutor`, `inbox-triage`,
`github-pr-reviewer`, `custom`.

Each assistant gets its own permission policy, model override, and
folder context — see [`docs/architecture.md`](../docs/architecture.md)
for the underlying state machine and capability model.
