import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { isPausableStatus, isPausedStatus } from "./pause_run";

describe("isPausableStatus", () => {
  it("accepts executing and awaiting_approval", () => {
    assert.equal(isPausableStatus("executing"), true);
    assert.equal(isPausableStatus("awaiting_approval"), true);
  });

  it("rejects paused and terminals", () => {
    for (const s of ["paused", "plan_review", "completed", "failed", "cancelled"]) {
      assert.equal(isPausableStatus(s), false, s);
    }
  });
});

describe("isPausedStatus", () => {
  it("matches only paused", () => {
    assert.equal(isPausedStatus("paused"), true);
    assert.equal(isPausedStatus("executing"), false);
  });
});
