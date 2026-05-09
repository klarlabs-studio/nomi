/**
 * UI-driven battle tests for surfaces the API specs can't fully
 * exercise: the approval-resolution dialog, the Memory tab CRUD,
 * and the Events tab live stream. These are the seams where a real
 * user actually clicks, so regressions here are user-facing even
 * when the backend tests stay green.
 */
import { expect } from "@playwright/test";
import type { APIRequestContext } from "@playwright/test";
import { test } from "./fixtures/auth";

interface RunDetail {
  run: { id: string; status: string };
  steps: Array<{ status: string; expected_capability?: string }>;
  plan?: { steps?: Array<{ expected_capability?: string }> };
}

async function pollRun(
  api: APIRequestContext,
  runID: string,
  predicate: (r: RunDetail) => boolean,
  timeoutMs = 120_000,
): Promise<RunDetail> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const r = await api.get(`/runs/${runID}`);
    if (r.status() === 200) {
      const j = (await r.json()) as RunDetail;
      if (predicate(j)) return j;
    }
    await new Promise((res) => setTimeout(res, 1000));
  }
  throw new Error(`run ${runID} never satisfied predicate within ${timeoutMs}ms`);
}

test.describe("approval dialog UI", () => {
  test.setTimeout(300_000);

  test("pending approval renders, Approve button resolves and resumes the run", async ({
    api,
    authedPage,
  }) => {
    // Pre-condition: a default LLM must exist. globalSetup wires the
    // FakeLLM as the default provider; if it doesn't, fail hard so we
    // notice the fixture is broken instead of silently skipping the
    // highest-value flows on every CI run.
    const settings = await api.get("/settings/llm-default");
    if (settings.status() !== 200 || !(await settings.json()).provider_id) {
      throw new Error("e2e fake-llm should be configured by globalSetup; check playwright.config.ts");
    }

    // Create assistant whose llm.chat is confirm-mode → first step
    // pauses for approval.
    const create = await api.post("/assistants", {
      data: {
        name: "Approval UI Test",
        role: "research",
        system_prompt:
          "Reply concisely. One short sentence. Do not use any tools.",
        capabilities: ["llm.chat"],
        permission_policy: {
          rules: [{ capability: "llm.chat", mode: "confirm" }],
        },
      },
    });
    const assistantID = (await create.json()).id as string;

    try {
      const runResp = await api.post("/runs", {
        data: { goal: "Say hello in 5 words.", assistant_id: assistantID },
      });
      const runID = (await runResp.json()).id as string;

      // Drive the run to plan_review and approve so executor reaches
      // the confirm-mode step.
      const planned = await pollRun(api, runID, (d) => d.run.status === "plan_review");
      // FakeLLM must produce a plan whose first step routes to llm.chat
      // so the confirm-mode rule fires. If it doesn't, the fixture
      // changed; fail hard and update the fixture.
      const planSteps = planned.plan?.steps ?? [];
      const nonLLM = planSteps.filter(
        (s) => s.expected_capability && s.expected_capability !== "llm.chat",
      );
      if (nonLLM.length > 0) {
        throw new Error(
          "FakeLLM produced a plan with non-llm.chat steps; update e2e/fixtures/fake-llm-server.mjs",
        );
      }
      await api.post(`/runs/${runID}/plan/approve`, { data: {} });

      // Run should pause for approval.
      await pollRun(api, runID, (d) => d.run.status === "awaiting_approval");

      // Open Approvals tab in the UI and find the pending approval.
      await authedPage.goto("/");
      await authedPage.click("#tab-approvals");

      // The approval card should mention the capability (llm.chat)
      // and have an Approve button. The button is initially "Wait..."
      // (armed timer); we wait for it to read "Approve".
      const approveBtn = authedPage.getByRole("button", { name: "Approve" });
      await expect(approveBtn).toBeVisible({ timeout: 15_000 });
      await expect(approveBtn).toBeEnabled({ timeout: 15_000 });
      await approveBtn.click();

      // Run resumes + completes.
      const terminal = await pollRun(
        api,
        runID,
        (d) =>
          d.run.status === "completed" ||
          d.run.status === "failed" ||
          d.run.status === "cancelled",
        180_000,
      );
      expect(terminal.run.status).toBe("completed");
    } finally {
      await api.delete(`/assistants/${assistantID}`);
    }
  });
});

test.describe("Memory tab CRUD", () => {
  test("create memory via UI form, see it listed, delete via UI", async ({
    api,
    authedPage,
  }) => {
    // The Memory UI has a single content input ("What should I
    // remember?") + a scope dropdown + a Save button. Memories are
    // free-text not key/value.
    const uniqueContent = `playwright e2e marker ${Date.now()}`;

    await authedPage.goto("/");
    await authedPage.click("#tab-memory");

    const contentInput = authedPage.getByPlaceholder("What should I remember?");
    await expect(contentInput).toBeVisible({ timeout: 5_000 });
    await contentInput.fill(uniqueContent);
    await authedPage.getByRole("button", { name: "Save" }).click();

    // The new memory should appear in the list.
    await expect(authedPage.getByText(uniqueContent).first()).toBeVisible({
      timeout: 5_000,
    });

    // Confirm via API too — UI says it's there, daemon should agree.
    const list = await api.get("/memory?limit=200").then((r) => r.json());
    const created = (list.memories as Array<{ id: string; content: string }>)
      .find((m) => m.content === uniqueContent);
    expect(created).toBeDefined();

    // Memory delete is direct (no confirm dialog) — onDelete calls
    // the API immediately. Find the matching row by its content
    // text and click its Delete button.
    const memoryRow = authedPage
      .locator('[class*="rounded"]')
      .filter({ hasText: uniqueContent })
      .first();
    await memoryRow.getByRole("button", { name: "Delete" }).click();

    // The memory should be gone from the API too.
    await expect
      .poll(async () => {
        const r = await api.get("/memory?limit=200").then((r) => r.json());
        return (r.memories as Array<{ content: string }>).some(
          (m) => m.content === uniqueContent,
        );
      })
      .toBe(false);
  });
});

test.describe("Events tab", () => {
  test("renders events from the run we just kicked", async ({
    api,
    authedPage,
  }) => {
    // Trigger a recognizable event by creating a run; the events
    // stream should surface a run.created event for it.
    const create = await api.post("/assistants", {
      data: {
        name: "Events Tab Test",
        role: "research",
        system_prompt: "Reply concisely.",
        capabilities: ["llm.chat"],
        permission_policy: { rules: [{ capability: "llm.chat", mode: "allow" }] },
      },
    });
    const assistantID = (await create.json()).id as string;
    try {
      const runResp = await api.post("/runs", {
        data: { goal: "ping", assistant_id: assistantID },
      });
      const runID = (await runResp.json()).id as string;

      await authedPage.goto("/");
      await authedPage.click("#tab-events");

      // The events list should mention the run id we just created
      // (or at least contain a run.created event). Use a generous
      // timeout because event delivery + render can take a beat.
      await expect(authedPage.getByText(/run\.created/i).first()).toBeVisible({
        timeout: 15_000,
      });
      // The run ID should appear in at least one event row.
      await expect(authedPage.getByText(runID).first()).toBeVisible({
        timeout: 15_000,
      });
    } finally {
      await api.delete(`/assistants/${assistantID}`);
    }
  });
});
