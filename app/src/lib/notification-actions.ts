/** Extra payload stamped on OS notifications. */
export const APPROVAL_NOTIF_EXTRA = { nomi: "approvals" } as const;
export const PLAN_NOTIF_EXTRA = { nomi: "plan" } as const;

export type NotificationActionKind = "approvals" | "plan";

/**
 * Classify a Tauri notification `onAction` / Web click payload.
 * Requires an explicit `extra.nomi` (or `action`) stamp.
 */
export function notificationActionKind(
  extra: Record<string, unknown> | undefined,
): NotificationActionKind | null {
  if (!extra) return null;
  if (extra.nomi === "approvals" || extra.action === "approvals") return "approvals";
  if (extra.nomi === "plan" || extra.action === "plan") return "plan";
  return null;
}

/** @deprecated Prefer `notificationActionKind` — kept for call-site clarity. */
export function isApprovalNotificationAction(
  extra: Record<string, unknown> | undefined,
): boolean {
  return notificationActionKind(extra) === "approvals";
}

/** Whether `plan.proposed` should fire an OS notification (initial only). */
export function shouldNotifyPlanProposed(payload: Record<string, unknown> | undefined): boolean {
  if (!payload) return true;
  if (payload.edited === true || payload.replan === true) return false;
  return true;
}
