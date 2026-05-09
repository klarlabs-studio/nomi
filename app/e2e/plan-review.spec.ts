import { test, expect, seedAssistant } from "./fixtures/auth";

/**
 * plan-review.spec.ts — drives the V1 flagship surface:
 *
 *   plan_review → /plan/edit → /plan/approve → executing
 *
 * Pre-refactor this whole path was tested only by smoke_test.go's
 * negative cases plus a one-line assertion in integration_test.go. The
 * UI flag-ship stayed unprotected against regressions.
 *
 * The test depends on the FakeLLM fixture being wired by globalSetup —
 * its deterministic plan (read README → llm.chat summarize) gives us a
 * stable two-step shape we can mutate via /plan/edit and re-approve.
 */
test.describe("plan-review flagship flow", () => {
  test("plan_review → edit → approve transitions run to executing", async ({ api }) => {
    const settings = await api.get("/settings/llm-default");
    if (settings.status() !== 200 || !(await settings.json()).provider_id) {
      throw new Error("e2e fake-llm should be configured by globalSetup");
    }

    // Seed an assistant that allows every capability the FakeLLM plan
    // routes through. Default skip-on-no-LLM specs use the same
    // helper; here we widen it to permit fs read + llm.chat.
    const { id: assistantID } = await seedAssistant(api, { mode: "allow" });
    // Widen the default rules so filesystem.read / llm.chat are
    // allowed without confirm. seedAssistant defaults command.exec +
    // filesystem.read; we add llm.chat:allow via PUT.
    await api.put(`/assistants/${assistantID}`, {
      data: {
        permission_policy: {
          rules: [
            { capability: "llm.chat", mode: "allow" },
            { capability: "filesystem.read", mode: "allow" },
            { capability: "command.exec", mode: "allow" },
          ],
        },
        capabilities: ["llm.chat", "filesystem.read"],
      },
    });

    // Kick the run; planner runs against FakeLLM.
    const runResp = await api.post("/runs", {
      data: { goal: "Read the README and summarize it.", assistant_id: assistantID },
    });
    expect(runResp.ok()).toBeTruthy();
    const runID = (await runResp.json()).id as string;

    // Wait for plan_review.
    const planned = await pollUntil(async () => {
      const r = await api.get(`/runs/${runID}`);
      if (!r.ok()) return null;
      const j = (await r.json()) as { run: { status: string }; plan?: { steps: PlanStep[] } };
      return j.run.status === "plan_review" ? j : null;
    }, 10_000);

    expect(planned, "run never reached plan_review").not.toBeNull();
    expect(planned!.plan?.steps?.length).toBe(2);
    expect(planned!.plan?.steps?.[0].expected_tool).toBe("filesystem.read");
    expect(planned!.plan?.steps?.[1].expected_tool).toBe("llm.chat");

    // Edit the plan: drop the read step, keep only the llm.chat
    // summary. This exercises the /plan/edit endpoint and proves the
    // edit propagates to the persisted plan.
    const editResp = await api.post(`/runs/${runID}/plan/edit`, {
      data: {
        steps: [
          {
            title: "Reply",
            description: "Answer directly.",
            expected_tool: "llm.chat",
            expected_capability: "llm.chat",
          },
        ],
      },
    });
    expect(editResp.ok(), `edit failed: ${await editResp.text()}`).toBeTruthy();

    // Re-fetch — the plan should now have one step.
    const afterEdit = await api.get(`/runs/${runID}`);
    const afterEditJ = (await afterEdit.json()) as { plan: { steps: PlanStep[]; version: number } };
    expect(afterEditJ.plan.steps).toHaveLength(1);
    expect(afterEditJ.plan.steps[0].expected_tool).toBe("llm.chat");
    expect(afterEditJ.plan.version).toBeGreaterThanOrEqual(2);

    // Approve and confirm the run advances out of plan_review.
    const approveResp = await api.post(`/runs/${runID}/plan/approve`, { data: {} });
    expect(approveResp.ok(), `approve failed: ${await approveResp.text()}`).toBeTruthy();

    const advanced = await pollUntil(async () => {
      const r = await api.get(`/runs/${runID}`);
      if (!r.ok()) return null;
      const j = (await r.json()) as { run: { status: string } };
      return j.run.status !== "plan_review" ? j : null;
    }, 10_000);
    expect(advanced, "run stayed in plan_review after approve").not.toBeNull();
    expect(["executing", "completed"]).toContain(advanced!.run.status);

    // Audit: at least two plan.proposed events must be in the chain —
    // one from the planner's first proposal, one from the user edit
    // (with edited=true). The runtime emits plan.proposed for both
    // cases so the audit chain records the user override even though
    // there isn't a dedicated plan.edited event type.
    const events = await api.get(`/events?run_id=${runID}&limit=50`);
    expect(events.ok()).toBeTruthy();
    const eventsJ = (await events.json()) as {
      events: Array<{ type: string; payload?: Record<string, unknown> }>;
    };
    const proposed = eventsJ.events.filter((e) => e.type === "plan.proposed");
    expect(proposed.length).toBeGreaterThanOrEqual(2);
    expect(proposed.some((e) => e.payload?.edited === true)).toBe(true);
  });
});

interface PlanStep {
  expected_tool?: string;
  expected_capability?: string;
}

// pollUntil polls fn until it returns a non-null value or the deadline
// passes, returning whatever fn returned (or null on timeout).
async function pollUntil<T>(fn: () => Promise<T | null>, timeoutMs: number): Promise<T | null> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const got = await fn();
    if (got !== null) return got;
    await new Promise((r) => setTimeout(r, 100));
  }
  return null;
}
