import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { formatPlanToastMessage, shouldShowPlanToast } from "./plan_toast";

describe("shouldShowPlanToast", () => {
  const base = { type: "plan.proposed", run_id: "run-1", payload: {} };

  it("shows for untracked initial proposals", () => {
    assert.equal(
      shouldShowPlanToast(base, {
        enabled: true,
        trackedIds: [],
        autoOpenEnabled: true,
        alreadyToasted: [],
      }),
      true,
    );
  });

  it("skips tracked when auto-open is on", () => {
    assert.equal(
      shouldShowPlanToast(base, {
        enabled: true,
        trackedIds: ["run-1"],
        autoOpenEnabled: true,
        alreadyToasted: [],
      }),
      false,
    );
  });

  it("shows tracked when auto-open is off", () => {
    assert.equal(
      shouldShowPlanToast(base, {
        enabled: true,
        trackedIds: ["run-1"],
        autoOpenEnabled: false,
        alreadyToasted: [],
      }),
      true,
    );
  });

  it("skips edits, replans, disabled, and duplicates", () => {
    assert.equal(
      shouldShowPlanToast(
        { ...base, payload: { edited: true } },
        { enabled: true, trackedIds: [], autoOpenEnabled: true, alreadyToasted: [] },
      ),
      false,
    );
    assert.equal(
      shouldShowPlanToast(base, {
        enabled: false,
        trackedIds: [],
        autoOpenEnabled: true,
        alreadyToasted: [],
      }),
      false,
    );
    assert.equal(
      shouldShowPlanToast(base, {
        enabled: true,
        trackedIds: [],
        autoOpenEnabled: true,
        alreadyToasted: ["run-1"],
      }),
      false,
    );
  });
});

describe("formatPlanToastMessage", () => {
  it("includes short run id", () => {
    assert.equal(
      formatPlanToastMessage("abcdefghij"),
      "Nomi: plan ready for review · run abcdefgh",
    );
  });
});
