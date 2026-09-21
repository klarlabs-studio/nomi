import type { NomiStreamEvent } from "./badge_events";

export type ApprovalToastInfo = {
  approvalId: string;
  capability: string;
  runId?: string;
};

/** Extract toast fields from an `approval.requested` SSE event. */
export function parseApprovalRequested(ev: NomiStreamEvent): ApprovalToastInfo | null {
  if (ev.type !== "approval.requested") return null;
  const approvalId =
    typeof ev.payload?.approval_id === "string" ? ev.payload.approval_id.trim() : "";
  if (!approvalId) return null;
  const capability =
    typeof ev.payload?.capability === "string" && ev.payload.capability.trim()
      ? ev.payload.capability.trim()
      : "tool";
  return {
    approvalId,
    capability,
    runId: ev.run_id,
  };
}

export function shouldShowApprovalToast(
  enabled: boolean,
  approvalId: string,
  alreadyToasted: Iterable<string>,
): boolean {
  if (!enabled || !approvalId) return false;
  return !new Set(alreadyToasted).has(approvalId);
}

export function formatApprovalToastMessage(info: ApprovalToastInfo): string {
  const run = info.runId ? ` · run ${info.runId.slice(0, 8)}` : "";
  return `Nomi: approve ${info.capability}?${run}`;
}
