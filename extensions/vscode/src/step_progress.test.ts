import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  isProgressEvent,
  shouldRevealProgress,
  StepProgressFormatter,
} from "./step_progress";

describe("isProgressEvent", () => {
  it("matches step and terminal run events", () => {
    assert.equal(isProgressEvent("step.started"), true);
    assert.equal(isProgressEvent("step.completed"), true);
    assert.equal(isProgressEvent("step.failed"), true);
    assert.equal(isProgressEvent("step.retrying"), true);
    assert.equal(isProgressEvent("plan.proposed"), true);
    assert.equal(isProgressEvent("run.completed"), true);
    assert.equal(isProgressEvent("run.paused"), true);
    assert.equal(isProgressEvent("run.resumed"), true);
  });

  it("ignores streaming and approval noise", () => {
    assert.equal(isProgressEvent("step.streaming"), false);
    assert.equal(isProgressEvent("approval.requested"), false);
    assert.equal(isProgressEvent("run.created"), false);
  });
});

describe("StepProgressFormatter", () => {
  it("formats started → completed with remembered title", () => {
    const f = new StepProgressFormatter();
    assert.equal(
      f.format({
        type: "step.started",
        run_id: "abcdefghij",
        step_id: "s1",
        payload: { title: "Read README" },
      }),
      "→ [abcdefgh] Read README",
    );
    assert.equal(
      f.format({
        type: "step.completed",
        run_id: "abcdefghij",
        step_id: "s1",
        payload: { output: "ok" },
      }),
      "✓ [abcdefgh] Read README",
    );
  });

  it("formats failed with error and retry", () => {
    const f = new StepProgressFormatter();
    f.format({
      type: "step.started",
      run_id: "r1xxxxxx",
      step_id: "s2",
      payload: { title: "Patch" },
    });
    assert.equal(
      f.format({
        type: "step.retrying",
        run_id: "r1xxxxxx",
        step_id: "s2",
      }),
      "↻ [r1xxxxxx] Patch (retry)",
    );
    assert.equal(
      f.format({
        type: "step.failed",
        run_id: "r1xxxxxx",
        step_id: "s2",
        payload: { error: "conflict" },
      }),
      "✗ [r1xxxxxx] Patch: conflict",
    );
  });

  it("formats plan and run terminal lines", () => {
    const f = new StepProgressFormatter();
    assert.equal(
      f.format({ type: "plan.proposed", run_id: "abc" }),
      "▶ [abc] plan ready for review",
    );
    assert.equal(
      f.format({ type: "run.paused", run_id: "abc" }),
      "⏸ [abc] paused",
    );
    assert.equal(
      f.format({ type: "run.resumed", run_id: "abc" }),
      "▶ [abc] resumed",
    );
    assert.equal(
      f.format({ type: "run.completed", run_id: "abc" }),
      "✓ [abc] done",
    );
  });
});

describe("shouldRevealProgress", () => {
  it("reveals only for tracked runs on key events", () => {
    const tracked = new Set(["run-1"]);
    assert.equal(
      shouldRevealProgress({ type: "step.started", run_id: "run-1" }, tracked),
      true,
    );
    assert.equal(
      shouldRevealProgress({ type: "step.started", run_id: "other" }, tracked),
      false,
    );
    assert.equal(
      shouldRevealProgress({ type: "step.completed", run_id: "run-1" }, tracked),
      false,
    );
  });
});
