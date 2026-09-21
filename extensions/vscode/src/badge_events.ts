/** Event types that should refresh the pending badge. */
export function isBadgeEvent(type: string): boolean {
  return (
    type.startsWith("approval.") ||
    type.startsWith("plan.") ||
    type === "run.cancelled"
  );
}

export type NomiStreamEvent = {
  id?: string;
  type: string;
  run_id?: string;
  step_id?: string;
  payload?: Record<string, unknown>;
};
