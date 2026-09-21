import type { NomiStreamEvent } from "./badge_events";

/**
 * Whether SSE should open the Plan Review panel without a keystroke.
 * Only the initial proposal for a tracked Ask Nomi / Review run —
 * edits and replans stay in the already-open panel / OutputChannel.
 */
export function shouldAutoOpenPlanReview(
  ev: NomiStreamEvent,
  trackedIds: Iterable<string>,
  enabled: boolean,
  alreadyOpened: Iterable<string>,
): boolean {
  if (!enabled) return false;
  if (ev.type !== "plan.proposed") return false;
  const runId = ev.run_id;
  if (!runId) return false;
  if (!new Set(trackedIds).has(runId)) return false;
  if (new Set(alreadyOpened).has(runId)) return false;
  const payload = ev.payload ?? {};
  if (payload.edited === true || payload.replan === true) return false;
  return true;
}
