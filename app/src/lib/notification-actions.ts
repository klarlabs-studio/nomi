/** Extra payload stamped on approval OS notifications. */
export const APPROVAL_NOTIF_EXTRA = { nomi: "approvals" } as const;

/**
 * Whether a Tauri notification `onAction` payload should open the
 * Approvals tab. Matches our stamped `extra.nomi` or any click when
 * we only send approval notifications through this helper.
 */
export function isApprovalNotificationAction(extra: Record<string, unknown> | undefined): boolean {
  if (!extra) return true; // our only sendNotification path is approvals
  if (extra.nomi === "approvals") return true;
  if (extra.action === "approvals") return true;
  return false;
}
