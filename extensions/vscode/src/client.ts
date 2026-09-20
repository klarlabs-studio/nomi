// Minimal REST client for the review surface (approvals + plan_review).
// Mirrors app/src/lib/api.ts + cmd/nomi — Bearer auth, no CORS concerns
// inside the extension host.

export interface Approval {
  id: string;
  run_id: string;
  step_id?: string;
  capability: string;
  status: string;
  created_at: string;
}

export interface Run {
  id: string;
  goal: string;
  status: string;
  assistant_id: string;
  created_at: string;
  updated_at: string;
}

export interface PendingSnapshot {
  approvals: Approval[];
  plans: Run[];
}

export interface EditorContextPayload {
  source: string;
  workspace_folders?: string[];
  open_tabs?: string[];
  active?: {
    path: string;
    language_id?: string;
    selection?: {
      start_line: number;
      end_line: number;
      text: string;
    };
  };
}

export interface AssistantSummary {
  id: string;
  name: string;
}

export class NomiClient {
  constructor(
    public readonly url: string,
    private readonly token: string,
    private readonly fetchImpl: typeof fetch = globalThis.fetch.bind(globalThis),
  ) {}

  private async request(method: string, path: string, body?: unknown): Promise<Response> {
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.token}`,
      Accept: "application/json",
    };
    let payload: string | undefined;
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    const res = await this.fetchImpl(`${this.url}${path}`, {
      method,
      headers,
      body: payload,
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`${method} ${path} → ${res.status}${text ? `: ${text.slice(0, 200)}` : ""}`);
    }
    return res;
  }

  async health(): Promise<boolean> {
    const res = await this.fetchImpl(`${this.url}/health`);
    return res.ok;
  }

  async listPendingApprovals(): Promise<Approval[]> {
    const res = await this.request("GET", "/approvals");
    const data = (await res.json()) as { approvals?: Approval[] };
    return data.approvals ?? [];
  }

  async listPlanReviewRuns(): Promise<Run[]> {
    const res = await this.request("GET", "/runs");
    const data = (await res.json()) as { runs?: Run[] };
    return (data.runs ?? []).filter((r) => r.status === "plan_review");
  }

  async snapshot(): Promise<PendingSnapshot> {
    const [approvals, plans] = await Promise.all([
      this.listPendingApprovals(),
      this.listPlanReviewRuns(),
    ]);
    return { approvals, plans };
  }

  async listAssistants(): Promise<AssistantSummary[]> {
    const res = await this.request("GET", "/assistants");
    const data = (await res.json()) as { assistants?: AssistantSummary[] };
    return data.assistants ?? [];
  }

  async createRun(
    goal: string,
    assistantId: string,
    editorContext?: EditorContextPayload,
  ): Promise<Run> {
    const body: Record<string, unknown> = {
      goal,
      assistant_id: assistantId,
    };
    if (editorContext) {
      body.editor_context = editorContext;
    }
    const res = await this.request("POST", "/runs", body);
    return (await res.json()) as Run;
  }

  async resolveApproval(id: string, approved: boolean): Promise<void> {
    await this.request("POST", `/approvals/${encodeURIComponent(id)}/resolve`, {
      approved,
      remember: false,
    });
  }

  async approvePlan(runId: string): Promise<void> {
    await this.request("POST", `/runs/${encodeURIComponent(runId)}/plan/approve`);
  }

  /** Deny a plan = cancel the run (same semantics as tray / channels). */
  async denyPlan(runId: string): Promise<void> {
    await this.request("POST", `/runs/${encodeURIComponent(runId)}/cancel`);
  }
}

export function pendingCount(snap: PendingSnapshot): number {
  return snap.approvals.length + snap.plans.length;
}
