import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { shouldAutoOpenPlanReview } from "./auto_open_plan";

describe("shouldAutoOpenPlanReview", () => {
  const base = { type: "plan.proposed", run_id: "run-1", payload: {} };

  it("opens for tracked initial proposals when enabled", () => {
    assert.equal(
      shouldAutoOpenPlanReview(base, ["run-1"], true, []),
      true,
    );
  });

  it("respects disabled setting", () => {
    assert.equal(
      shouldAutoOpenPlanReview(base, ["run-1"], false, []),
      false,
    );
  });

  it("ignores untracked and non-proposal events", () => {
    assert.equal(
      shouldAutoOpenPlanReview(base, ["other"], true, []),
      false,
    );
    assert.equal(
      shouldAutoOpenPlanReview(
        { type: "step.started", run_id: "run-1" },
        ["run-1"],
        true,
        [],
      ),
      false,
    );
  });

  it("skips edits, replans, and already-opened runs", () => {
    assert.equal(
      shouldAutoOpenPlanReview(
        { ...base, payload: { edited: true } },
        ["run-1"],
        true,
        [],
      ),
      false,
    );
    assert.equal(
      shouldAutoOpenPlanReview(
        { ...base, payload: { replan: true } },
        ["run-1"],
        true,
        [],
      ),
      false,
    );
    assert.equal(
      shouldAutoOpenPlanReview(base, ["run-1"], true, ["run-1"]),
      false,
    );
  });
});
