import type { NomiStreamEvent } from "./badge_events";

/** Events that belong on the live step-progress OutputChannel. */
export function isProgressEvent(type: string): boolean {
  return (
    type === "step.started" ||
    type === "step.completed" ||
    type === "step.failed" ||
    type === "step.retrying" ||
    type === "plan.proposed" ||
    type === "run.completed" ||
    type === "run.failed" ||
    type === "run.cancelled"
  );
}

/**
 * Formats SSE events into CLI-parity progress lines (`→` / `✓` / `✗` / `↻`).
 * Remembers step titles from `step.started` so later events can label them.
 */
export class StepProgressFormatter {
  private readonly titles = new Map<string, string>();

  format(ev: NomiStreamEvent): string | null {
    if (!isProgressEvent(ev.type)) return null;
    const run = (ev.run_id ?? "?").slice(0, 8);
    const sid = typeof ev.step_id === "string" ? ev.step_id : "";
    switch (ev.type) {
      case "step.started": {
        const title =
          typeof ev.payload?.title === "string" && ev.payload.title.trim()
            ? ev.payload.title.trim()
            : "step";
        if (sid) this.titles.set(sid, title);
        return `→ [${run}] ${title}`;
      }
      case "step.completed": {
        const title = (sid && this.titles.get(sid)) || "step";
        if (sid) this.titles.delete(sid);
        return `✓ [${run}] ${title}`;
      }
      case "step.failed": {
        const title = (sid && this.titles.get(sid)) || "step";
        if (sid) this.titles.delete(sid);
        const err =
          typeof ev.payload?.error === "string" ? ev.payload.error.trim() : "";
        return err ? `✗ [${run}] ${title}: ${err}` : `✗ [${run}] ${title}`;
      }
      case "step.retrying": {
        const title = (sid && this.titles.get(sid)) || "step";
        return `↻ [${run}] ${title} (retry)`;
      }
      case "plan.proposed":
        return `▶ [${run}] plan ready for review`;
      case "run.completed":
        this.clearRun(ev.run_id);
        return `✓ [${run}] done`;
      case "run.failed":
        this.clearRun(ev.run_id);
        return `✗ [${run}] failed`;
      case "run.cancelled":
        this.clearRun(ev.run_id);
        return `✗ [${run}] cancelled`;
      default:
        return null;
    }
  }

  private clearRun(runId: string | undefined): void {
    if (!runId) return;
    // Titles are keyed by step id only; nothing to clear by run.
    // Kept for future multi-run bookkeeping.
    void runId;
  }
}

/** Whether this event should reveal the OutputChannel for a tracked run. */
export function shouldRevealProgress(
  ev: NomiStreamEvent,
  trackedRunIds: ReadonlySet<string>,
): boolean {
  if (!ev.run_id || trackedRunIds.size === 0) return false;
  if (!trackedRunIds.has(ev.run_id)) return false;
  return (
    ev.type === "step.started" ||
    ev.type === "plan.proposed" ||
    ev.type === "run.completed" ||
    ev.type === "run.failed" ||
    ev.type === "run.cancelled"
  );
}
