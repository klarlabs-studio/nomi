/** Statuses ManualReplan is meant for (CLI `isReplanableStatus`). */
export function isReplanableStatus(status: string): boolean {
  return status === "failed" || status === "cancelled";
}
