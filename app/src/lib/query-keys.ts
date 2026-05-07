/**
 * Centralized React Query cache keys for Nomi. Keeping them in one file makes
 * two things cleaner:
 *
 *   1. Components can't accidentally diverge ("runs.list" vs "runs-list").
 *   2. The event→invalidation mapping in EventProvider has a single reference.
 *
 * Convention: every key is an array whose first element is the resource
 * namespace. Sub-keys narrow by ID.
 */
export const queryKeys = {
  runs: {
    all: ["runs"] as const,
    list: () => ["runs", "list"] as const,
    detail: (id: string) => ["runs", "detail", id] as const,
    approvals: (id: string) => ["runs", id, "approvals"] as const,
  },
  assistants: {
    all: ["assistants"] as const,
    list: () => ["assistants", "list"] as const,
  },
  approvals: {
    all: ["approvals"] as const,
    list: () => ["approvals", "list"] as const,
  },
  memory: {
    all: ["memory"] as const,
    list: (params?: { scope?: string; q?: string }) =>
      ["memory", "list", params ?? {}] as const,
  },
  events: {
    all: ["events"] as const,
    list: (params?: { run_id?: string; type?: string }) =>
      ["events", "list", params ?? {}] as const,
  },
  providers: {
    all: ["providers"] as const,
    list: () => ["providers", "list"] as const,
  },
  connectors: {
    all: ["connectors"] as const,
    configs: () => ["connectors", "configs"] as const,
    statuses: () => ["connectors", "statuses"] as const,
  },
  health: {
    all: ["health"] as const,
    check: () => ["health", "check"] as const,
  },
  plugins: {
    all: ["plugins"] as const,
    list: () => ["plugins", "list"] as const,
    configs: () => ["plugins", "configs"] as const,
  },
};
