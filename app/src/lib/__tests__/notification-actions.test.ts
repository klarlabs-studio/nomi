import { describe, expect, it } from "vitest";
import {
  isApprovalNotificationAction,
  notificationActionKind,
  shouldNotifyPlanProposed,
} from "../notification-actions";

describe("notificationActionKind", () => {
  it("classifies stamped extras", () => {
    expect(notificationActionKind(undefined)).toBe(null);
    expect(notificationActionKind({ nomi: "approvals" })).toBe("approvals");
    expect(notificationActionKind({ nomi: "plan" })).toBe("plan");
    expect(notificationActionKind({ action: "plan" })).toBe("plan");
  });

  it("rejects unrelated extras", () => {
    expect(notificationActionKind({ nomi: "other" })).toBe(null);
  });
});

describe("isApprovalNotificationAction", () => {
  it("matches only approvals stamps", () => {
    expect(isApprovalNotificationAction({ nomi: "approvals" })).toBe(true);
    expect(isApprovalNotificationAction({ nomi: "plan" })).toBe(false);
    expect(isApprovalNotificationAction(undefined)).toBe(false);
  });
});

describe("shouldNotifyPlanProposed", () => {
  it("skips edits and replans", () => {
    expect(shouldNotifyPlanProposed(undefined)).toBe(true);
    expect(shouldNotifyPlanProposed({})).toBe(true);
    expect(shouldNotifyPlanProposed({ edited: true })).toBe(false);
    expect(shouldNotifyPlanProposed({ replan: true })).toBe(false);
  });
});
