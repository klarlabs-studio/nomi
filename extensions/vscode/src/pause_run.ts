/** Statuses Runtime.PauseRun accepts (mirrors CLI `isPausableStatus`). */
export function isPausableStatus(status: string): boolean {
  return status === "executing" || status === "awaiting_approval";
}

export function isPausedStatus(status: string): boolean {
  return status === "paused";
}
