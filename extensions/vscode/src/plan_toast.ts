import type { NomiStreamEvent } from "./badge_events";

export type PlanToastInfo = {
  runId: string;
};

/** Initial plan.proposed for runs that won't auto-open Plan Review. */
export function shouldShowPlanToast(
  ev: NomiStreamEvent,
  opts: {
    enabled: boolean;
    trackedIds: Iterable<string>;
    autoOpenEnabled: boolean;
    alreadyToasted: Iterable<string>;
  },
): boolean {
  if (!opts.enabled) return false;
  if (ev.type !== "plan.proposed") return false;
  const runId = ev.run_id;
  if (!runId) return false;
  if (new Set(opts.alreadyToasted).has(runId)) return false;
  const payload = ev.payload ?? {};
  if (payload.edited === true || payload.replan === true) return false;
  // Tracked + auto-open → panel opens; toast would be redundant.
  if (new Set(opts.trackedIds).has(runId) && opts.autoOpenEnabled) {
    return false;
  }
  return true;
}

export function formatPlanToastMessage(runId: string): string {
  return `Nomi: plan ready for review · run ${runId.slice(0, 8)}`;
}
