import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { isCancelableStatus, preferCancelCandidates } from "./cancel_run";

describe("isCancelableStatus", () => {
  it("accepts non-terminal run statuses", () => {
    for (const s of [
      "created",
      "planning",
      "plan_review",
      "awaiting_approval",
      "executing",
      "paused",
    ]) {
      assert.equal(isCancelableStatus(s), true, s);
    }
  });

  it("rejects terminal statuses", () => {
    for (const s of ["completed", "failed", "cancelled"]) {
      assert.equal(isCancelableStatus(s), false, s);
    }
  });
});

describe("preferCancelCandidates", () => {
  const runs = [
    { id: "a", goal: "one", status: "executing" },
    { id: "b", goal: "two", status: "planning" },
    { id: "c", goal: "three", status: "plan_review" },
  ];

  it("returns all cancelable when nothing is tracked", () => {
    assert.deepEqual(preferCancelCandidates(runs, []), runs);
  });

  it("prefers tracked runs that are still cancelable", () => {
    assert.deepEqual(preferCancelCandidates(runs, ["b", "x"]), [runs[1]]);
  });

  it("falls back to all cancelable when tracked are gone", () => {
    assert.deepEqual(preferCancelCandidates(runs, ["done-already"]), runs);
  });
});
