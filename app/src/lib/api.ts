import { invoke } from "@tauri-apps/api/core";

import type {
  Run,
  RunWithSteps,
  Assistant,
  AssistantBinding,
  BindingRole,
  ChannelIdentity,
  Conversation,
  Event,
  Approval,
  Memory,
  ConnectorManifest,
  ConnectorStatus,
  ConnectorConfig,
  MarketplaceCatalog,
  Plugin,
  PluginConnection,
  PluginState,
  ProviderProfile,
  ProviderProfileRequest,
  LLMDefaultSettings,
  OnboardingStatus,
  SafetyProfileSettings,
  CreateRunRequest,
  CreateAssistantRequest,
  CreateMemoryRequest,
  TriggerRule,
  RemoteTemplate,
  ApiError as ApiErrorType,
} from "@/types/api";
import {
  ApprovalListSchema,
  MemoryListSchema,
  RunDetailSchema,
  RunListSchema,
} from "@/types/schemas";

// Hard fallback used outside Tauri (e.g. vite preview, Playwright preview
// server) where get_api_endpoint isn't reachable. Inside Tauri the actual
// URL is read from `~/Library/Application Support/Nomi/api.endpoint` via
// the get_api_endpoint command — that file is written by nomid at startup
// with whatever port `app_settings.api_port` resolved to.
const API_BASE_FALLBACK = "http://127.0.0.1:8080";

// The bearer token is fetched lazily on the first request and cached for the
// lifetime of the renderer. The daemon writes the token file at startup, so by
// the time any user action triggers a fetch the token is available.
let tokenPromise: Promise<string> | null = null;
let apiBasePromise: Promise<string> | null = null;

/**
 * Test/dev environments (Playwright running against `vite preview`) have no
 * Tauri bridge, so `invoke("get_auth_token")` throws. As a fallback, the e2e
 * harness injects the real token into `window.__NOMI_DEV_TOKEN__` via
 * page.addInitScript — the web layer falls back to it here. In a Tauri
 * production build invoke succeeds on the first call and this branch is
 * never reached.
 */
declare global {
  interface Window {
    __NOMI_DEV_TOKEN__?: string;
  }
}

function getApiBase(): Promise<string> {
  if (!apiBasePromise) {
    const inTauri =
      typeof window !== "undefined" &&
      typeof (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ !==
        "undefined";
    apiBasePromise = (async () => {
      if (!inTauri) {
        return API_BASE_FALLBACK;
      }
      try {
        const url = await invoke<string>("get_api_endpoint");
        return url && url.length > 0 ? url : API_BASE_FALLBACK;
      } catch {
        // Outside Tauri (vite preview, Playwright). The fallback URL
        // matches the historic hardcoded default so e2e against
        // `vite preview` against `nomid` on :8080 keeps working.
        return API_BASE_FALLBACK;
      }
    })();
  }
  return apiBasePromise;
}

// resetAuthState clears cached token + endpoint promises so the next
// request resolves them fresh. Called on 401 responses so a daemon
// restart (which rotates the token file) doesn't brick the renderer
// until the user reloads the window.
function resetAuthState(): void {
  tokenPromise = null;
  apiBasePromise = null;
}

function getAuthToken(): Promise<string> {
  if (!tokenPromise) {
    // Outside Tauri the bridge global (__TAURI_INTERNALS__) is
    // missing; some shim versions then hang waiting for a transport
    // that never replies, which freezes every fetch. Short-circuit
    // to the dev-token fallback so vite preview / Playwright / Scout
    // reach the daemon without waiting on a no-op IPC.
    const inTauri =
      typeof window !== "undefined" &&
      typeof (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ !==
        "undefined";

    tokenPromise = (async () => {
      if (!inTauri) {
        const devToken =
          typeof window !== "undefined" ? window.__NOMI_DEV_TOKEN__ : undefined;
        if (devToken) {
          return devToken;
        }
        tokenPromise = null;
        throw new Error("no auth token (Tauri bridge unavailable + no dev token)");
      }
      try {
        return await invoke<string>("get_auth_token");
      } catch (err) {
        const devToken =
          typeof window !== "undefined" ? window.__NOMI_DEV_TOKEN__ : undefined;
        if (devToken) {
          return devToken;
        }
        tokenPromise = null;
        throw err;
      }
    })();
  }
  return tokenPromise;
}

export class ApiError extends Error {
  /**
   * Parsed JSON body from the failing response, when available. Carries
   * structured fields (code, violations, suggested_capabilities, ...)
   * the UI can act on — e.g. the assistant editor renders a one-click
   * "apply suggested capabilities" button when code === "ceiling_violation".
   * `undefined` means the response wasn't JSON.
   */
  public readonly body: Record<string, unknown> | undefined;

  constructor(public status: number, message: string, body?: Record<string, unknown>) {
    super(message);
    this.name = "ApiError";
    this.body = body;
  }
}

async function fetchApi<T>(path: string, options?: RequestInit): Promise<T> {
  const send = async (): Promise<Response> => {
    let token: string;
    let base: string;
    try {
      [token, base] = await Promise.all([getAuthToken(), getApiBase()]);
    } catch (err) {
      throw new ApiError(
        0,
        `Failed to load API auth token: ${err instanceof Error ? err.message : String(err)}`
      );
    }

    const headers = new Headers(options?.headers);
    if (!headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }
    headers.set("Authorization", `Bearer ${token}`);

    try {
      return await fetch(`${base}${path}`, { ...options, headers });
    } catch (err) {
      // Only network-level failures (fetch rejected) reach this branch. Surface
      // the original message so the UI can distinguish "daemon not running" from
      // other failure modes.
      throw new ApiError(
        0,
        `Network error reaching ${path}: ${err instanceof Error ? err.message : String(err)}`
      );
    }
  };

  let response = await send();
  if (response.status === 401) {
    // The daemon may have restarted and rotated its token. Drop the cached
    // promises and retry once with fresh credentials before surfacing the
    // error to callers.
    resetAuthState();
    response = await send();
  }

  if (!response.ok) {
    let message = `HTTP ${response.status}`;
    let parsedBody: Record<string, unknown> | undefined;
    try {
      const body = (await response.json()) as ApiErrorType & Record<string, unknown>;
      parsedBody = body as Record<string, unknown>;
      if (body?.error) {
        message = body.error;
      }
    } catch {
      // Body wasn't JSON — fall back to status text.
      message = response.statusText || message;
    }
    throw new ApiError(response.status, message, parsedBody);
  }

  return response.json() as Promise<T>;
}

// fetchApiParsed validates the response body against a zod schema before
// returning it. A schema mismatch surfaces as a typed ApiError("schema
// mismatch...") with the parse issues attached, which beats a confused
// render or a silent NaN when the daemon's wire shape drifts. Use this for
// boundary calls where the body shape is load-bearing (run/plan/approval
// state machines, memory entries). Plain `fetchApi` remains for the
// long-tail endpoints where validation hasn't paid for itself yet.
async function fetchApiParsed<S extends import("zod").ZodTypeAny>(
  path: string,
  schema: S,
  options?: RequestInit,
): Promise<import("zod").z.infer<S>> {
  const body = await fetchApi<unknown>(path, options);
  const result = schema.safeParse(body);
  if (!result.success) {
    const issues = result.error.issues
      .map((i) => `${i.path.join(".") || "(root)"}: ${i.message}`)
      .join("; ");
    throw new ApiError(0, `schema mismatch on ${path}: ${issues}`, { issues });
  }
  return result.data;
}

async function fetchApiText(path: string, options?: RequestInit): Promise<string> {
  const send = async (): Promise<Response> => {
    let token: string;
    let base: string;
    try {
      [token, base] = await Promise.all([getAuthToken(), getApiBase()]);
    } catch (err) {
      throw new ApiError(
        0,
        `Failed to load API auth token: ${err instanceof Error ? err.message : String(err)}`
      );
    }
    const headers = new Headers(options?.headers);
    headers.set("Authorization", `Bearer ${token}`);

    try {
      return await fetch(`${base}${path}`, { ...options, headers });
    } catch (err) {
      throw new ApiError(
        0,
        `Network error reaching ${path}: ${err instanceof Error ? err.message : String(err)}`
      );
    }
  };

  let response = await send();
  if (response.status === 401) {
    resetAuthState();
    response = await send();
  }
  if (!response.ok) {
    throw new ApiError(response.status, response.statusText || `HTTP ${response.status}`);
  }
  return response.text();
}

// Run API
export const runsApi = {
  create: (data: CreateRunRequest) =>
    fetchApi<Run>("/runs", {
      method: "POST",
      body: JSON.stringify(data),
    }),

  get: (id: string) =>
    // Validated at the boundary so a daemon-side wire change shows up as a
    // typed schema-mismatch error in the chat detail panel rather than as
    // a render of undefined fields.
    fetchApiParsed(`/runs/${id}`, RunDetailSchema) as Promise<RunWithSteps>,

  list: () =>
    fetchApiParsed("/runs", RunListSchema) as Promise<{ runs: Run[] }>,

  approve: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/approve`, {
      method: "POST",
    }),

  approvePlan: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/plan/approve`, {
      method: "POST",
    }),

  editPlan: (
    id: string,
    steps: {
      id?: string;
      title: string;
      description?: string;
      expected_tool?: string;
      expected_capability?: string;
      depends_on?: string[];
      // arguments lets the desktop UI push a modified tool argument
      // payload (e.g. a unified diff with skipped hunks dropped) so
      // the planner doesn't have to re-run.
      arguments?: Record<string, unknown>;
    }[]
  ) =>
    fetchApi<{ status: string }>(`/runs/${id}/plan/edit`, {
      method: "POST",
      body: JSON.stringify({ steps }),
    }),

  fork: (id: string, step_id: string, goal?: string) =>
    fetchApi<{ run: Run }>(`/runs/${id}/fork`, {
      method: "POST",
      body: JSON.stringify({ step_id, goal }),
    }),

  retry: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/retry`, {
      method: "POST",
    }),

  // replan: ask the planner to propose a corrective plan against the
  // last failed step. Surfaced in the UI as "Fix this with the agent".
  // Bounded server-side by MaxReplansPerRun.
  replan: (id: string) =>
    fetchApi<{ status: string; step_count: number }>(`/runs/${id}/replan`, {
      method: "POST",
    }),

  pause: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/pause`, {
      method: "POST",
    }),

  resume: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/resume`, {
      method: "POST",
    }),

  cancel: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}/cancel`, {
      method: "POST",
    }),

  getApprovals: (id: string) =>
    fetchApi<{ approvals: Approval[] }>(`/runs/${id}/approvals`),

  delete: (id: string) =>
    fetchApi<{ status: string }>(`/runs/${id}`, {
      method: "DELETE",
    }),
};

// Assistant API
export const assistantsApi = {
  create: (data: CreateAssistantRequest) =>
    fetchApi<Assistant>("/assistants", {
      method: "POST",
      body: JSON.stringify(data),
    }),

  get: (id: string) => fetchApi<Assistant>(`/assistants/${id}`),

  list: () => fetchApi<{ assistants: Assistant[] }>("/assistants"),

  listTemplates: () => fetchApi<{ templates: Assistant[] }>("/assistants/templates"),

  update: (id: string, data: CreateAssistantRequest) =>
    fetchApi<Assistant>(`/assistants/${id}`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),

  applySafetyProfile: (id: string) =>
    fetchApi<{ status: string; profile: string; assistant: Assistant }>(
      `/assistants/${id}/apply-safety-profile`,
      {
        method: "POST",
      },
    ),

  delete: (id: string) =>
    fetchApi<{ status: string }>(`/assistants/${id}`, {
      method: "DELETE",
    }),
};

// Recipe registry API (roady #125). The built-in catalog ships with
// the daemon binary; user-imported and exported recipes live in the
// recipes table on the SQLite store. Install creates a fresh
// assistant from the recipe; export bundles an existing assistant
// back into a shareable YAML manifest.
export interface RecipeCatalogEntry {
  id: string;
  name: string;
  version: string;
  author?: string;
  description?: string;
  tags?: string[];
  source: "builtin" | "imported" | "exported";
  sha256?: string;
}

export interface RecipeAssistantSpec {
  name: string;
  tagline?: string;
  role: string;
  best_for?: string;
  not_for?: string;
  suggested_model?: string;
  system_prompt: string;
  capabilities?: string[];
  memory_policy?: unknown;
  permission_policy?: unknown;
  executor_backend?: string;
  sandbox_image?: string;
}

export interface RecipeDocument {
  schema_version: number;
  id: string;
  name: string;
  version: string;
  author?: string;
  description?: string;
  tags?: string[];
  assistant: RecipeAssistantSpec;
}

export const recipesApi = {
  list: () => fetchApi<{ recipes: RecipeCatalogEntry[] }>("/recipes"),
  get: (id: string) =>
    fetchApi<{ recipe: RecipeDocument; sha256: string; source: string }>(`/recipes/${id}`),
  preview: (id: string) =>
    fetchApi<{
      recipe: RecipeDocument;
      sha256: string;
      assistant_preview: Record<string, unknown>;
    }>(`/recipes/${id}/preview`),
  install: (id: string, expected_sha256?: string) =>
    fetchApi<{ assistant: Assistant; recipe_id: string; sha256: string }>("/recipes/install", {
      method: "POST",
      body: JSON.stringify({ id, expected_sha256 }),
    }),
  export: (assistantId: string, id?: string, version?: string) =>
    fetchApi<{ recipe: RecipeDocument; sha256: string; yaml: string }>(
      `/recipes/export?assistant_id=${encodeURIComponent(assistantId)}`,
      {
        method: "POST",
        body: JSON.stringify({ id, version }),
      },
    ),
};

// Schedules — cron-driven Runs (roady #124). The /translate endpoint
// turns a natural-language phrase into a cron expression via the
// configured LLM provider; the UI is expected to surface the parsed
// result for confirmation before calling create.
export interface Schedule {
  id: string;
  assistant_id: string;
  prompt: string;
  cron_expr: string;
  nl_phrase?: string;
  enabled: boolean;
  next_fire_at: string;
  last_fire_at?: string;
  last_run_id?: string;
  last_error?: string;
  created_at: string;
}

export interface TranslateResult {
  nl_phrase: string;
  cron_expr: string;
  explanation: string;
  valid: boolean;
}

export const schedulesApi = {
  list: () => fetchApi<{ schedules: Schedule[] }>("/schedules"),
  get: (id: string) => fetchApi<Schedule>(`/schedules/${id}`),
  create: (params: {
    assistant_id: string;
    prompt: string;
    cron_expr: string;
    nl_phrase?: string;
    enabled?: boolean;
  }) =>
    fetchApi<Schedule>("/schedules", {
      method: "POST",
      body: JSON.stringify(params),
    }),
  patch: (
    id: string,
    params: { prompt?: string; cron_expr?: string; enabled?: boolean },
  ) =>
    fetchApi<Schedule>(`/schedules/${id}`, {
      method: "PATCH",
      body: JSON.stringify(params),
    }),
  delete: (id: string) =>
    fetchApi<{ status: string }>(`/schedules/${id}`, { method: "DELETE" }),
  translate: (phrase: string) =>
    fetchApi<TranslateResult>("/schedules/translate", {
      method: "POST",
      body: JSON.stringify({ phrase }),
    }),
};

// Skills (roady #126) — induction reads the user's past successful
// runs, heuristically clusters them by goal-text similarity, and
// surfaces candidate Recipes. Promote materialises the suggestion as
// a Recipe + Assistant via the same install path.
export interface SkillSuggestion {
  id: string;
  representative_goal: string;
  common_tokens?: string[];
  source_run_ids: string[];
  size: number;
  suggested_assistant_id?: string;
}

export interface SynthesizedRecipe {
  suggested_name: string;
  suggested_role?: string;
  system_prompt: string;
  capabilities: string[];
  explanation?: string;
}

export const skillsApi = {
  listSuggestions: () =>
    fetchApi<{ suggestions: SkillSuggestion[] }>("/skills/suggestions"),
  promote: (params: {
    suggestion_id: string;
    recipe_id: string;
    name: string;
    description?: string;
    source_assistant_id?: string;
    synthesized_role?: string;
    synthesized_system_prompt?: string;
    synthesized_capabilities?: string[];
  }) =>
    fetchApi<{
      recipe: RecipeDocument;
      sha256: string;
      assistant: Assistant;
      source_run_ids: string[];
    }>("/skills/promote", {
      method: "POST",
      body: JSON.stringify(params),
    }),
  synthesize: (suggestionID: string) =>
    fetchApi<{ suggestion: SkillSuggestion; recipe: SynthesizedRecipe }>(
      "/skills/synthesize",
      {
        method: "POST",
        body: JSON.stringify({ suggestion_id: suggestionID }),
      },
    ),
};

// Runtime introspection — surfaces what backends nomid registered at
// boot so the assistant editor can populate its Sandbox dropdown
// instead of hardcoding ["local", "docker", "gvisor"]. Backends only
// show up here when nomid successfully probed them at startup, so the
// dropdown won't offer Docker on a machine where the daemon isn't running.
export const runtimeApi = {
  executorBackends: () =>
    fetchApi<{ backends: string[] }>("/runtime/executor-backends"),
};

// Event API
export const eventsApi = {
  list: (params?: { run_id?: string; type?: string; limit?: number }) => {
    const searchParams = new URLSearchParams();
    if (params?.run_id) searchParams.append("run_id", params.run_id);
    if (params?.type) searchParams.append("type", params.type);
    if (params?.limit) searchParams.append("limit", params.limit.toString());

    return fetchApi<{ events: Event[] }>(`/events?${searchParams}`);
  },
};

// Approval API
export const approvalsApi = {
  list: () =>
    fetchApiParsed("/approvals", ApprovalListSchema) as Promise<{
      approvals: Approval[];
    }>,

  resolve: (id: string, approved: boolean, remember = false) =>
    fetchApi<{ status: string; approved: boolean }>(`/approvals/${id}/resolve`, {
      method: "POST",
      body: JSON.stringify({ approved, remember }),
    }),
};

// Memory API
export const memoryApi = {
  list: (params?: { scope?: string; q?: string; limit?: number }) => {
    const searchParams = new URLSearchParams();
    if (params?.scope) searchParams.append("scope", params.scope);
    if (params?.q) searchParams.append("q", params.q);
    if (params?.limit) searchParams.append("limit", params.limit.toString());
    return fetchApiParsed(`/memory?${searchParams}`, MemoryListSchema) as Promise<{
      memories: Memory[];
    }>;
  },

  create: (data: CreateMemoryRequest) =>
    fetchApi<Memory>("/memory", {
      method: "POST",
      body: JSON.stringify(data),
    }),

  get: (id: string) => fetchApi<Memory>(`/memory/${id}`),

  delete: (id: string) =>
    fetchApi<{ status: string }>(`/memory/${id}`, {
      method: "DELETE",
    }),
};

// Tools API
export const toolsApi = {
  previewFolderContext: (path: string, maxDepth?: number) =>
    fetchApi<{ path: string; tree: unknown; stats: unknown }>(
      "/tools/filesystem.context/preview",
      {
        method: "POST",
        body: JSON.stringify({ path, max_depth: maxDepth || 3 }),
      }
    ),
};

// Connector API
export const connectorsApi = {
  list: () => fetchApi<{ connectors: ConnectorManifest[] }>("/connectors"),

  listStatuses: () =>
    fetchApi<{ statuses: ConnectorStatus[] }>("/connectors/statuses"),

  getStatus: (name: string) =>
    fetchApi<ConnectorStatus>(`/connectors/${name}/status`),

  listConfigs: () =>
    fetchApi<{ connectors: ConnectorConfig[] }>("/connectors/configs"),

  updateConfig: (
    name: string,
    data: { config: Record<string, unknown>; enabled: boolean }
  ) =>
    fetchApi<{ status: string }>(`/connectors/${name}/config`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),
};

// Remote assistant templates marketplace
export const remoteTemplatesApi = {
  list: () => fetchApi<{ templates: RemoteTemplate[] }>("/remote-templates"),
  install: (template: RemoteTemplate) =>
    fetchApi<{ assistant_id: string; status: string }>("/remote-templates/install", {
      method: "POST",
      body: JSON.stringify(template),
    }),
};

// Plugins API (ADR 0001). First-class endpoints replacing the legacy
// /connectors/... surface. The Plugins tab consumes these.
export const pluginsApi = {
  list: () => fetchApi<{ plugins: Plugin[] }>("/plugins"),

  get: (id: string) => fetchApi<Plugin>(`/plugins/${encodeURIComponent(id)}`),

  createConnection: (
    pluginID: string,
    data: {
      name: string;
      config?: Record<string, unknown>;
      credentials?: Record<string, string>;
      enabled?: boolean;
    },
  ) =>
    fetchApi<PluginConnection>(`/plugins/${encodeURIComponent(pluginID)}/connections`, {
      method: "POST",
      body: JSON.stringify(data),
    }),

  updateConnection: (
    pluginID: string,
    connectionID: string,
    data: {
      name?: string;
      config?: Record<string, unknown>;
      credentials?: Record<string, string>;
      enabled?: boolean;
      webhook_enabled?: boolean;
      webhook_event_allowlist?: string[];
    },
  ) =>
    fetchApi<PluginConnection>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}`,
      {
        method: "PATCH",
        body: JSON.stringify(data),
      },
    ),

  deleteConnection: (pluginID: string, connectionID: string) =>
    fetchApi<{ status: string }>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}`,
      { method: "DELETE" },
    ),

  // Webhook management (Tunneled Inbound Receiver)
  rotateWebhookSecret: (connectionID: string) =>
    fetchApi<{ status: string }>(
      `/webhook-admin/${encodeURIComponent(connectionID)}/rotate-secret`,
      { method: "POST" },
    ),

  updateWebhookAllowlist: (connectionID: string, allowlist: string[]) =>
    fetchApi<{ allowlist: string[] }>(
      `/webhook-admin/${encodeURIComponent(connectionID)}/allowlist`,
      {
        method: "PUT",
        body: JSON.stringify({ allowlist }),
      },
    ),

  // Email trigger rules (task-email-plugin). Nested under
  // /plugins/:id/connections/:conn_id/trigger-rules.
  triggerRules: {
    list: (pluginID: string, connectionID: string) =>
      fetchApi<{ rules: TriggerRule[] }>(
        `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/trigger-rules`,
      ),

    create: (pluginID: string, connectionID: string, rule: TriggerRule) =>
      fetchApi<{ rule: TriggerRule }>(
        `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/trigger-rules`,
        {
          method: "POST",
          body: JSON.stringify(rule),
        },
      ),

    update: (pluginID: string, connectionID: string, name: string, rule: TriggerRule) =>
      fetchApi<{ rule: TriggerRule }>(
        `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/trigger-rules/${encodeURIComponent(name)}`,
        {
          method: "PUT",
          body: JSON.stringify(rule),
        },
      ),

    delete: (pluginID: string, connectionID: string, name: string) =>
      fetchApi<{ status: string }>(
        `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/trigger-rules/${encodeURIComponent(name)}`,
        { method: "DELETE" },
      ),
  },

  getTunnelStatus: () =>
    fetchApi<{ enabled: boolean; public_url: string }>("/webhook-admin/tunnel"),

  // Plugin lifecycle state (ADR 0002 §1). Enable/disable in v1; install,
  // uninstall, marketplace, and update arrived in lifecycle-07/09/10.
  getState: (pluginID: string) =>
    fetchApi<PluginState>(`/plugins/${encodeURIComponent(pluginID)}/state`),

  setEnabled: (pluginID: string, enabled: boolean) =>
    fetchApi<PluginState>(`/plugins/${encodeURIComponent(pluginID)}/state`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    }),

  setEnabledRoles: (pluginID: string, roles: string[]) =>
    fetchApi<PluginState>(`/plugins/${encodeURIComponent(pluginID)}/state`, {
      method: "PATCH",
      body: JSON.stringify({ enabled_roles: roles }),
    }),

  // Marketplace install (lifecycle-07). The daemon accepts either a
  // JSON {url} body or a multipart upload with field name `bundle`.
  // Two methods so the call site picks the right shape — bundling
  // them under one method would force multipart on the URL path or
  // hide the file API behind an awkward "either-or" type.
  installFromURL: (url: string) =>
    fetchApi<Plugin>("/plugins/install", {
      method: "POST",
      body: JSON.stringify({ url }),
    }),

  installFromFile: async (file: File) => {
    // FormData must NOT have an explicit Content-Type — the browser
    // sets it (with the multipart boundary). fetchApi defaults to
    // application/json which would corrupt the upload, so this path
    // bypasses fetchApi and uses a raw fetch with manually managed
    // headers.
    const form = new FormData();
    form.append("bundle", file);
    const send = async (): Promise<Response> => {
      const [token, base] = await Promise.all([getAuthToken(), getApiBase()]);
      return fetch(`${base}/plugins/install`, {
        method: "POST",
        body: form,
        headers: { Authorization: `Bearer ${token}` },
      });
    };
    let resp = await send();
    if (resp.status === 401) {
      resetAuthState();
      resp = await send();
    }
    if (!resp.ok) {
      let message = `HTTP ${resp.status}`;
      try {
        const body = (await resp.json()) as ApiErrorType;
        if (body?.error) message = body.error;
      } catch {
        message = resp.statusText || message;
      }
      throw new ApiError(resp.status, message);
    }
    return (await resp.json()) as Plugin;
  },

  // Marketplace uninstall (lifecycle-07). cascade=true also wipes
  // connection rows + secrets; default false preserves them so a
  // future reinstall reattaches.
  uninstall: (pluginID: string, cascade = false) =>
    fetchApi<{ status: string; cascade: boolean }>(
      `/plugins/${encodeURIComponent(pluginID)}${cascade ? "?cascade=true" : ""}`,
      { method: "DELETE" },
    ),

  // NomiHub catalog (lifecycle-09). Returns the latest verified
  // catalog from the daemon's in-memory cache.
  marketplace: () => fetchApi<MarketplaceCatalog>("/plugins/marketplace"),

  // Update an installed plugin to the catalog's latest version
  // (lifecycle-10). Synchronous: response holds the new state row
  // once the swap completes.
  update: (pluginID: string) =>
    fetchApi<PluginState>(`/plugins/${encodeURIComponent(pluginID)}/update`, {
      method: "POST",
    }),
};

// Channel identity allowlist (ADR 0001 §9). Per-(plugin, connection)
// entries that gate inbound senders. UI surfaces the list inside a
// connection's expanded view on the Plugins tab.
export const identitiesApi = {
  list: (pluginID: string, connectionID: string) =>
    fetchApi<{ identities: ChannelIdentity[] }>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/identities`,
    ),

  create: (
    pluginID: string,
    connectionID: string,
    data: {
      external_identifier: string;
      display_name?: string;
      allowed_assistants?: string[];
      enabled: boolean;
    },
  ) =>
    fetchApi<ChannelIdentity>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/identities`,
      {
        method: "POST",
        body: JSON.stringify(data),
      },
    ),

  update: (
    pluginID: string,
    connectionID: string,
    identityID: string,
    data: { display_name?: string; allowed_assistants?: string[]; enabled?: boolean },
  ) =>
    fetchApi<ChannelIdentity>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/identities/${encodeURIComponent(identityID)}`,
      {
        method: "PATCH",
        body: JSON.stringify(data),
      },
    ),

  delete: (pluginID: string, connectionID: string, identityID: string) =>
    fetchApi<{ status: string }>(
      `/plugins/${encodeURIComponent(pluginID)}/connections/${encodeURIComponent(connectionID)}/identities/${encodeURIComponent(identityID)}`,
      { method: "DELETE" },
    ),
};

// Assistant → Connection bindings (ADR 0001 §4). Used by the agent
// builder view (plugin-ui-02).
export const assistantBindingsApi = {
  list: (assistantID: string) =>
    fetchApi<{ bindings: AssistantBinding[] }>(
      `/assistants/${encodeURIComponent(assistantID)}/bindings`,
    ),

  upsert: (
    assistantID: string,
    data: {
      connection_id: string;
      role: BindingRole;
      enabled: boolean;
      is_primary?: boolean;
      priority?: number;
    },
  ) =>
    fetchApi<{ status: string }>(`/assistants/${encodeURIComponent(assistantID)}/bindings`, {
      method: "POST",
      body: JSON.stringify(data),
    }),

  delete: (assistantID: string, connectionID: string, role: BindingRole) =>
    fetchApi<{ status: string }>(
      `/assistants/${encodeURIComponent(assistantID)}/bindings/${encodeURIComponent(connectionID)}/${encodeURIComponent(role)}`,
      { method: "DELETE" },
    ),
};

// Conversations API — persistent multi-turn threads (ADR 0001 §8).
// Conversations are created by channel plugins, not by REST clients;
// this surface is read-mostly (+ delete for housekeeping).
export const conversationsApi = {
  listByAssistant: (assistantID: string) =>
    fetchApi<{ conversations: Conversation[] }>(`/conversations?assistant_id=${encodeURIComponent(assistantID)}`),

  listByConnection: (connectionID: string) =>
    fetchApi<{ conversations: Conversation[] }>(`/conversations?connection_id=${encodeURIComponent(connectionID)}`),

  get: (id: string) =>
    fetchApi<Conversation>(`/conversations/${encodeURIComponent(id)}`),

  delete: (id: string) =>
    fetchApi<{ status: string }>(`/conversations/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
};

// Provider Profile API
export const providersApi = {
  list: () => fetchApi<{ profiles: ProviderProfile[] }>("/provider-profiles"),

  get: (id: string) => fetchApi<ProviderProfile>(`/provider-profiles/${id}`),

  create: (data: ProviderProfileRequest) =>
    fetchApi<ProviderProfile>("/provider-profiles", {
      method: "POST",
      body: JSON.stringify(data),
    }),

  update: (id: string, data: ProviderProfileRequest) =>
    fetchApi<ProviderProfile>(`/provider-profiles/${id}`, {
      method: "PUT",
      body: JSON.stringify(data),
    }),

  delete: (id: string) =>
    fetchApi<{ status: string }>(`/provider-profiles/${id}`, {
      method: "DELETE",
    }),
};

// Settings API
export const settingsApi = {
  getLLMDefault: () => fetchApi<LLMDefaultSettings>("/settings/llm-default"),

  setLLMDefault: (data: LLMDefaultSettings) =>
    fetchApi<LLMDefaultSettings>("/settings/llm-default", {
      method: "PUT",
      body: JSON.stringify(data),
    }),

  getOnboardingComplete: () =>
    fetchApi<OnboardingStatus>("/settings/onboarding-complete"),

  setOnboardingComplete: (complete: boolean) =>
    fetchApi<OnboardingStatus>("/settings/onboarding-complete", {
      method: "PUT",
      body: JSON.stringify({ complete }),
    }),

  getSafetyProfile: () => fetchApi<SafetyProfileSettings>("/settings/safety-profile"),

  setSafetyProfile: (profile: SafetyProfileSettings["profile"]) =>
    fetchApi<SafetyProfileSettings>("/settings/safety-profile", {
      method: "PUT",
      body: JSON.stringify({ profile }),
    }),
};

// Health check
export const healthApi = {
  check: () => fetchApi<{ status: string }>("/health"),
};

export interface BuildInfo {
  version: string;
  commit: string;
  build_date: string;
}

// Build info — the daemon's GET /version (public path, no token needed
// but fetchApi will attach one if available) and the Tauri shell's
// `app_version` command. The two are independent because the shell
// and the daemon ship as separate binaries with their own version
// stamps. The About panel surfaces both.
export const versionApi = {
  daemon: () => fetchApi<BuildInfo>("/version"),

  // The shell version comes from the Tauri command. In non-Tauri
  // environments (vite preview, Playwright) `invoke` throws synchronously
  // before its returned promise can reject, so wrap in an async IIFE
  // and surface a deterministic fallback.
  shell: async (): Promise<BuildInfo> => {
    try {
      return await invoke<BuildInfo>("app_version");
    } catch {
      return { version: "unknown", commit: "none", build_date: "unknown" };
    }
  },
};

export const auditApi = {
  export: (params: {
    from: string;
    to: string;
    format?: "json" | "ndjson";
    redact?: boolean;
  }) => {
    const search = new URLSearchParams({
      from: params.from,
      to: params.to,
      format: params.format || "ndjson",
      redact: params.redact ? "true" : "false",
    });
    return fetchApiText(`/audit/export?${search.toString()}`);
  },

  prune: (days: number) =>
    fetchApi<{ status: string; deleted: number; cutoff: string; retention_days: number }>(
      "/audit/prune",
      {
        method: "POST",
        body: JSON.stringify({ days }),
      },
    ),
};
