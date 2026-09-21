import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  formatApprovalToastMessage,
  parseApprovalRequested,
  shouldShowApprovalToast,
} from "./approval_toast";

describe("parseApprovalRequested", () => {
  it("reads approval_id and capability", () => {
    assert.deepEqual(
      parseApprovalRequested({
        type: "approval.requested",
        run_id: "run-1",
        payload: { approval_id: "ap-1", capability: "filesystem.write" },
      }),
      { approvalId: "ap-1", capability: "filesystem.write", runId: "run-1" },
    );
  });

  it("returns null for other events or missing id", () => {
    assert.equal(parseApprovalRequested({ type: "plan.proposed" }), null);
    assert.equal(
      parseApprovalRequested({
        type: "approval.requested",
        payload: { capability: "x" },
      }),
      null,
    );
  });
});

describe("shouldShowApprovalToast", () => {
  it("respects setting and dedupe", () => {
    assert.equal(shouldShowApprovalToast(false, "ap-1", []), false);
    assert.equal(shouldShowApprovalToast(true, "ap-1", []), true);
    assert.equal(shouldShowApprovalToast(true, "ap-1", ["ap-1"]), false);
  });
});

describe("formatApprovalToastMessage", () => {
  it("includes capability and short run id", () => {
    assert.equal(
      formatApprovalToastMessage({
        approvalId: "ap",
        capability: "command.exec",
        runId: "abcdefghij",
      }),
      "Nomi: approve command.exec? · run abcdefgh",
    );
  });
});
