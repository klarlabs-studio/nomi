# Recipe: Research Assistant

Read papers, summarize threads, draft notes — all anchored to a single
folder of PDFs / markdown / source material on your laptop. The agent
plans the read order, summarizes per-source, and writes a synthesized
draft to a file you approve.

## What it does

- Reads any file in the research folder (`filesystem.read`,
  `filesystem.context`).
- Asks the LLM to summarize, compare, and synthesize across sources.
- Drafts notes to disk only after a plan-review approval
  (`filesystem.write` gated `confirm`).
- Memory entries (`workspace` scope) capture insights so follow-up
  prompts can reference them without re-reading.

## Apply

```bash
nomi seed examples/research-assistant/seed.yaml
```

Then open the desktop app, pick "Researcher", and try:

- **"Summarize the three PDFs in this folder. Note the assumptions
  each one makes about cache invalidation."**
- **"What does each paper say about durability vs. throughput?
  Write a comparison table to `notes/cache-tradeoffs.md`."**
- **"Find citations across the folder that mention CRDT merge
  semantics. Group by paper."**

## Files

- [`seed.yaml`](seed.yaml) — provider + Researcher assistant +
  workspace pointer.
