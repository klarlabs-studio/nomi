// fake-llm-server.mjs — deterministic LLM stand-in for Playwright e2e.
//
// Speaks the OpenAI-compatible /v1/chat/completions surface that the
// nomid Ollama / OpenAI-compatible adapter dials. Two response classes:
//
//   - planner request (system prompt mentions "planning assistant for
//     Nomi"): returns a fixed two-step plan (read README → summarize).
//   - everything else: short canned chat reply so step execution can
//     finish without a real model.
//
// Spawned by playwright.config.ts as a webServer entry so its lifecycle
// runs alongside the vite preview. Listens on FAKE_LLM_PORT (default
// 21434).
//
// Plain .mjs so it runs under node directly without tsx / ts-node.

import { createServer } from "node:http";

const port = Number(process.env.FAKE_LLM_PORT ?? 21434);

const DEFAULT_PLAN = JSON.stringify({
  steps: [
    {
      title: "Read README",
      description: "Inspect README.md.",
      tool: "filesystem.read",
      arguments: { path: "README.md" },
    },
    {
      title: "Summarize",
      description: "Five-bullet summary.",
      tool: "llm.chat",
      arguments: { prompt: "Summarize what you just read in 5 bullets." },
    },
  ],
});

// Single-step llm.chat fallback for assistants whose declared
// capabilities don't include filesystem.read. Used when the planner
// prompt only lists llm.chat under "Available tools:".
const LLM_ONLY_PLAN = JSON.stringify({
  steps: [
    {
      title: "Reply",
      description: "Answer the user goal directly.",
      tool: "llm.chat",
      arguments: { prompt: "Reply concisely to the user's goal." },
    },
  ],
});

// pickPlan inspects the planner prompt body. The runtime emits one
// `- <toolname>: <desc>` line per permitted tool. If filesystem.read
// isn't in the catalog, the assistant has no fs capability and we
// must emit an llm.chat-only plan or the runtime will reject the plan
// at the per-step capability ceiling.
function pickPlan(body) {
  const hasFSRead = /^- filesystem\.read:/m.test(body);
  return hasFSRead ? DEFAULT_PLAN : LLM_ONLY_PLAN;
}

const server = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    res.setHeader("Content-Type", "application/json");
    const isPlanner = body.includes("planning assistant for Nomi");
    const content = isPlanner ? pickPlan(body) : "ok";
    const payload = {
      model: "fake-model",
      choices: [{ message: { role: "assistant", content } }],
    };
    res.statusCode = 200;
    res.end(JSON.stringify(payload));
  });
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(`fake-llm ready at http://127.0.0.1:${port}\n`);
});

const shutdown = () => {
  server.close(() => process.exit(0));
};
process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);
