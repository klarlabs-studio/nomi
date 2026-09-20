import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { Plan } from "./client.js";
import {
  formatPlanReview,
  keepPlanSteps,
  planRequiresCaution,
  summarizeDiff,
  toEditPlanSteps,
} from "./plan_fmt.js";

describe("summarizeDiff", () => {
  it("counts lines and files", () => {
    const diff = `--- a/foo.ts
+++ b/foo.ts
@@ -1,2 +1,3 @@
 keep
-old
+new
+more
`;
    const s = summarizeDiff(diff);
    assert.deepEqual(s.files, ["foo.ts"]);
    assert.equal(s.added, 2);
    assert.equal(s.removed, 1);
  });
});

describe("planRequiresCaution", () => {
  it("flags write and patch", () => {
    const plan: Plan = {
      id: "p1",
      version: 1,
      steps: [
        {
          id: "s1",
          title: "patch",
          expected_tool: "filesystem.patch",
          order: 0,
          arguments: { diff: "--- a/x\n+++ b/x\n+hi\n" },
        },
      ],
    };
    assert.equal(planRequiresCaution(plan), true);
  });

  it("ignores read-only", () => {
    const plan: Plan = {
      id: "p1",
      version: 1,
      steps: [
        {
          id: "s1",
          title: "read",
          expected_tool: "filesystem.read",
          expected_capability: "filesystem.read",
          order: 0,
          arguments: { path: "/tmp/a" },
        },
      ],
    };
    assert.equal(planRequiresCaution(plan), false);
  });
});

describe("formatPlanReview", () => {
  it("includes goal, steps, and diff body", () => {
    const plan: Plan = {
      id: "p1",
      version: 1,
      steps: [
        {
          id: "s1",
          title: "Apply fix",
          expected_tool: "filesystem.patch",
          expected_capability: "filesystem.write",
          why: "user asked",
          order: 0,
          arguments: {
            diff: "--- a/a.ts\n+++ b/a.ts\n@@ -1 +1 @@\n-old\n+new\n",
          },
        },
      ],
    };
    const text = formatPlanReview("fix the bug", plan);
    assert.match(text, /Goal: fix the bug/);
    assert.match(text, /Apply fix/);
    assert.match(text, /diff: \+1 −1/);
    assert.match(text, /\+new/);
    assert.match(text, /writes files/);
  });
});

describe("keepPlanSteps / toEditPlanSteps", () => {
  it("drops unchecked steps and cleans depends_on", () => {
    const plan: Plan = {
      id: "p1",
      version: 1,
      steps: [
        { id: "a", title: "One", expected_tool: "filesystem.read", order: 0 },
        {
          id: "b",
          title: "Two",
          expected_tool: "filesystem.write",
          depends_on: ["a"],
          order: 1,
        },
        {
          id: "c",
          title: "Three",
          expected_tool: "filesystem.read",
          depends_on: ["a", "b"],
          order: 2,
        },
      ],
    };
    const kept = keepPlanSteps(plan, [0, 2]);
    assert.deepEqual(
      kept.steps.map((s) => s.id),
      ["a", "c"],
    );
    assert.deepEqual(kept.steps[1]!.depends_on, ["a"]);
    const body = toEditPlanSteps(kept);
    assert.equal(body.length, 2);
    assert.equal(body[0]!.id, "a");
    assert.equal(body[1]!.expected_tool, "filesystem.read");
  });

  it("rejects dropping every step", () => {
    const plan: Plan = {
      id: "p1",
      version: 1,
      steps: [{ id: "a", title: "One", order: 0 }],
    };
    assert.throws(() => keepPlanSteps(plan, []), /deny/);
  });
});
