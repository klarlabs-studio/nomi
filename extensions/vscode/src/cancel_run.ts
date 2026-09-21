/** Statuses a user can still cancel (mirrors CLI `isCancelableStatus`). */
const CANCELABLE = new Set([
  "created",
  "planning",
  "plan_review",
  "awaiting_approval",
  "executing",
  "paused",
]);

export function isCancelableStatus(status: string): boolean {
  return CANCELABLE.has(status);
}

export interface CancelCandidate {
  id: string;
  goal: string;
  status: string;
}

/**
 * Prefer Ask Nomi / Plan Review tracked runs when they are still
 * cancelable; otherwise fall back to every cancelable run from the API.
 */
export function preferCancelCandidates(
  cancelable: CancelCandidate[],
  trackedIds: Iterable<string>,
): CancelCandidate[] {
  const tracked = new Set(trackedIds);
  if (tracked.size === 0) return cancelable;
  const preferred = cancelable.filter((r) => tracked.has(r.id));
  return preferred.length > 0 ? preferred : cancelable;
}
