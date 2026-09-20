import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { Plan } from "./client.js";
import {
  formatPlanReview,
  planRequiresCaution,
  summarizeDiff,
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
