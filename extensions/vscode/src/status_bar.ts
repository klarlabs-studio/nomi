import type { NomiStreamEvent } from "./badge_events";

export type LiveStepKind = "running" | "retrying" | "paused" | "plan";

export type LiveStep = {
  runId: string;
  title: string;
  kind: LiveStepKind;
};

function stepTitle(ev: NomiStreamEvent, known?: string): string {
  if (typeof ev.payload?.title === "string" && ev.payload.title.trim()) {
    return ev.payload.title.trim();
  }
  return known?.trim() || "step";
}

/**
 * Derive the ambient status-bar step from SSE. Only tracks runs in
 * `trackedRunIds` (Ask Nomi / Plan Review) so channel noise stays off
 * the badge. Pending approvals still win in `formatStatusBarText`.
 */
export function updateLiveStep(
  ev: NomiStreamEvent,
  trackedRunIds: ReadonlySet<string>,
  current: LiveStep | null,
  /** Remembered titles from StepProgressFormatter / prior started. */
  titles: ReadonlyMap<string, string>,
): LiveStep | null {
  const runId = ev.run_id;
  if (!runId || !trackedRunIds.has(runId)) return current;

  const sid = typeof ev.step_id === "string" ? ev.step_id : "";
  switch (ev.type) {
    case "step.started":
      return { runId, title: stepTitle(ev), kind: "running" };
    case "step.retrying":
      return {
        runId,
        title: stepTitle(ev, sid ? titles.get(sid) : undefined),
        kind: "retrying",
      };
    case "step.completed":
    case "step.failed":
      // Clear only if this was the active live step's run.
      return current?.runId === runId ? null : current;
    case "plan.proposed":
      return { runId, title: "plan ready", kind: "plan" };
    case "run.paused":
      return { runId, title: "paused", kind: "paused" };
    case "run.resumed":
      return { runId, title: current?.runId === runId ? current.title : "running", kind: "running" };
    case "run.completed":
    case "run.failed":
    case "run.cancelled":
      return current?.runId === runId ? null : current;
    default:
      return current;
  }
}

export function formatStatusBarText(pending: number, live: LiveStep | null): string {
  if (pending > 0) {
    return `$(shield) Nomi ${pending}`;
  }
  if (live) {
    const title = truncateTitle(live.title, 28);
    switch (live.kind) {
      case "running":
        return `$(sync~spin) Nomi → ${title}`;
      case "retrying":
        return `$(sync~spin) Nomi ↻ ${title}`;
      case "paused":
        return `$(debug-pause) Nomi paused`;
      case "plan":
        return `$(list-tree) Nomi plan`;
    }
  }
  return "$(shield) Nomi";
}

/** Click target: pending → showPending; live step → showProgress. */
export function statusBarCommand(pending: number, live: LiveStep | null): string {
  if (pending > 0) return "nomi.showPending";
  if (live) return "nomi.showProgress";
  return "nomi.showPending";
}

function truncateTitle(title: string, max: number): string {
  const t = title.trim();
  if (t.length <= max) return t;
  return `${t.slice(0, max - 1)}…`;
}
