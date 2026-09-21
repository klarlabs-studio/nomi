import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { isReplanableStatus } from "./replan_run";

describe("isReplanableStatus", () => {
  it("accepts failed and cancelled", () => {
    assert.equal(isReplanableStatus("failed"), true);
    assert.equal(isReplanableStatus("cancelled"), true);
  });

  it("rejects active and completed", () => {
    for (const s of ["executing", "plan_review", "completed", "paused"]) {
      assert.equal(isReplanableStatus(s), false, s);
    }
  });
});
