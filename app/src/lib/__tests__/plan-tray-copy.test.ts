import { describe, expect, it } from "vitest";

import { planTrayCopy } from "@/lib/plan-tray-copy";
import type { Plan, Run } from "@/types/api";

function run(goal = "Ship the feature"): Run {
  return {
    id: "run-1",
    goal,
    assistant_id: "asst-1",
    status: "plan_review",
    plan_version: 1,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function plan(steps: Plan["steps"]): Plan {
  return {
    id: "plan-1",
    run_id: "run-1",
    version: 1,
    steps,
    created_at: "2026-01-01T00:00:00Z",
  };
}

describe("planTrayCopy", () => {
  it("summarizes from the run goal", () => {
    const copy = planTrayCopy(run("Fix the flaky test"), plan([]));
    expect(copy.summary).toBe("Fix the flaky test");
  });

  it("forces irreversible for filesystem.write steps", () => {
    const copy = planTrayCopy(
      run(),
      plan([
        {
          id: "s1",
          plan_id: "plan-1",
          title: "Write file",
          expected_tool: "filesystem.write",
          expected_capability: "filesystem.write",
          order: 0,
          created_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    expect(copy.dangerSignal).toBe("irreversible");
  });

  it("forces irreversible for filesystem.patch steps", () => {
    const copy = planTrayCopy(
      run(),
      plan([
        {
          id: "s1",
          plan_id: "plan-1",
          title: "Apply patch",
          expected_tool: "filesystem.patch",
          expected_capability: "filesystem.write",
          order: 0,
          created_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    expect(copy.dangerSignal).toBe("irreversible");
  });

  it("allows safe read-only plans without irreversible", () => {
    const copy = planTrayCopy(
      run(),
      plan([
        {
          id: "s1",
          plan_id: "plan-1",
          title: "Read README",
          expected_tool: "filesystem.read",
          expected_capability: "filesystem.read",
          order: 0,
          created_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    expect(copy.dangerSignal).toBeUndefined();
  });

  it("forces irreversible for mutate-shaped MCP tools", () => {
    const copy = planTrayCopy(
      run(),
      plan([
        {
          id: "s1",
          plan_id: "plan-1",
          title: "Write via MCP",
          expected_tool: "mcp.docs.write_file",
          expected_capability: "mcp.docs.write_file",
          order: 0,
          created_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    expect(copy.dangerSignal).toBe("irreversible");
  });
});
