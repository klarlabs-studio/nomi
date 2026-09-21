import { describe, expect, it } from "vitest";
import { isApprovalNotificationAction } from "../notification-actions";

describe("isApprovalNotificationAction", () => {
  it("matches stamped extra and empty payload", () => {
    expect(isApprovalNotificationAction(undefined)).toBe(true);
    expect(isApprovalNotificationAction({ nomi: "approvals" })).toBe(true);
    expect(isApprovalNotificationAction({ action: "approvals" })).toBe(true);
  });

  it("rejects unrelated extras", () => {
    expect(isApprovalNotificationAction({ nomi: "other" })).toBe(false);
  });
});
