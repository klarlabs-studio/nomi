import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  formatStatusBarText,
  statusBarCommand,
  updateLiveStep,
  type LiveStep,
} from "./status_bar";

describe("updateLiveStep", () => {
  const tracked = new Set(["run-1"]);
  const titles = new Map([["s1", "Apply patch"]]);

  it("sets running on step.started for tracked runs", () => {
    const next = updateLiveStep(
      {
        type: "step.started",
        run_id: "run-1",
        step_id: "s1",
        payload: { title: "Apply patch" },
      },
      tracked,
      null,
      titles,
    );
    assert.deepEqual(next, { runId: "run-1", title: "Apply patch", kind: "running" });
  });

  it("ignores untracked runs", () => {
    const cur: LiveStep = { runId: "run-1", title: "x", kind: "running" };
    const next = updateLiveStep(
      { type: "step.started", run_id: "other", payload: { title: "Nope" } },
      tracked,
      cur,
      titles,
    );
    assert.equal(next, cur);
  });

  it("clears on step.completed / run terminal for the active run", () => {
    const cur: LiveStep = { runId: "run-1", title: "Apply patch", kind: "running" };
    assert.equal(
      updateLiveStep(
        { type: "step.completed", run_id: "run-1", step_id: "s1" },
        tracked,
        cur,
        titles,
      ),
      null,
    );
    assert.equal(
      updateLiveStep({ type: "run.completed", run_id: "run-1" }, tracked, cur, titles),
      null,
    );
  });

  it("marks paused and plan", () => {
    assert.deepEqual(
      updateLiveStep({ type: "run.paused", run_id: "run-1" }, tracked, null, titles),
      { runId: "run-1", title: "paused", kind: "paused" },
    );
    assert.deepEqual(
      updateLiveStep({ type: "plan.proposed", run_id: "run-1" }, tracked, null, titles),
      { runId: "run-1", title: "plan ready", kind: "plan" },
    );
  });
});

describe("formatStatusBarText", () => {
  it("prefers plan chrome over pending count", () => {
    assert.equal(
      formatStatusBarText(2, { runId: "r", title: "plan ready", kind: "plan" }),
      "$(list-tree) Nomi plan",
    );
  });

  it("prefers pending count over running step", () => {
    assert.equal(
      formatStatusBarText(2, { runId: "r", title: "Patch", kind: "running" }),
      "$(shield) Nomi 2",
    );
  });

  it("shows spinner + title when live and idle pending", () => {
    assert.equal(
      formatStatusBarText(0, { runId: "r", title: "Apply patch", kind: "running" }),
      "$(sync~spin) Nomi → Apply patch",
    );
    assert.equal(formatStatusBarText(0, null), "$(shield) Nomi");
  });
});

describe("statusBarCommand", () => {
  it("routes plan to Plan Review and paused to Resume", () => {
    assert.equal(
      statusBarCommand(1, { runId: "r", title: "plan ready", kind: "plan" }),
      "nomi.openStatusPlan",
    );
    assert.equal(
      statusBarCommand(0, { runId: "r", title: "paused", kind: "paused" }),
      "nomi.resumeRun",
    );
  });

  it("routes pending or progress otherwise", () => {
    assert.equal(statusBarCommand(1, null), "nomi.showPending");
    assert.equal(
      statusBarCommand(0, { runId: "r", title: "x", kind: "running" }),
      "nomi.showProgress",
    );
  });
});
