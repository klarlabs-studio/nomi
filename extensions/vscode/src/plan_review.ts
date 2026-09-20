import * as vscode from "vscode";
import type { NomiClient, Plan, PlanStep, Run } from "./client";
import { listHunks, type HunkListItem } from "./diff_hunks";
import {
  applyHunkSkips,
  formatPlanReview,
  keepPlanSteps,
  planRequiresCaution,
  toEditPlanSteps,
  truncateChars,
} from "./plan_fmt";

export type PlanReviewCallbacks = {
  onResolved: () => void | Promise<void>;
};

type EditMessage = {
  type?: string;
  keep?: number[];
  /** Unchecked hunk keys → skip (desktop DiffPreview parity). */
  skippedHunks?: Record<string, string[]>;
};

/**
 * Plan+diff review panel: drop steps and skip patch hunks via /plan/edit.
 * Shiki / side-by-side stay on the desktop DiffPreview.
 */
export async function openPlanReview(
  client: NomiClient,
  run: Run,
  callbacks: PlanReviewCallbacks,
): Promise<void> {
  let detail = await client.getRun(run.id);
  let plan: Plan | null = detail.plan;
  let goal = detail.run?.goal ?? run.goal;

  const panel = vscode.window.createWebviewPanel(
    "nomiPlanReview",
    `Nomi Plan · ${run.id.slice(0, 8)}`,
    vscode.ViewColumn.Beside,
    { enableScripts: true, retainContextWhenHidden: true },
  );

  const render = (): void => {
    const caution = planRequiresCaution(plan);
    const body = formatPlanReview(goal, plan);
    const nonce = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    panel.webview.html = renderHtml(panel.webview, body, caution, plan?.steps ?? [], nonce);
  };
  render();

  const sub = panel.webview.onDidReceiveMessage(async (msg: EditMessage) => {
    try {
      if (msg.type === "approve") {
        await client.approvePlan(run.id);
        vscode.window.showInformationMessage("Nomi: plan approved");
        panel.dispose();
        await callbacks.onResolved();
      } else if (msg.type === "deny") {
        await client.denyPlan(run.id);
        vscode.window.showInformationMessage("Nomi: plan denied (run cancelled)");
        panel.dispose();
        await callbacks.onResolved();
      } else if (msg.type === "edit") {
        if (!plan || plan.steps.length === 0) {
          vscode.window.showWarningMessage("Nomi: nothing to edit");
          return;
        }
        const keep = Array.isArray(msg.keep) ? msg.keep : [];
        if (keep.length === 0) {
          vscode.window.showWarningMessage("Nomi: keep at least one step (or Deny)");
          return;
        }
        let nextPlan =
          keep.length === plan.steps.length ? plan : keepPlanSteps(plan, keep);
        const skippedByStep = msg.skippedHunks ?? {};
        const { steps: editedSteps, hunksChanged } = applyHunkSkips(nextPlan, skippedByStep);
        nextPlan = { ...nextPlan, steps: editedSteps };

        const stepsDropped = keep.length !== plan.steps.length;
        if (!stepsDropped && !hunksChanged) {
          vscode.window.showInformationMessage("Nomi: no changes to apply");
          return;
        }

        await client.editPlan(run.id, toEditPlanSteps(nextPlan));
        detail = await client.getRun(run.id);
        plan = detail.plan;
        goal = detail.run?.goal ?? goal;
        const parts: string[] = [];
        if (stepsDropped) parts.push(`${nextPlan.steps.length} step(s) kept`);
        if (hunksChanged) parts.push("hunks updated");
        vscode.window.showInformationMessage(`Nomi: plan updated — ${parts.join(", ")}`);
        render();
      }
    } catch (err) {
      vscode.window.showErrorMessage(
        `Nomi: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  });
  panel.onDidDispose(() => sub.dispose());
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function stepCheckboxRow(s: PlanStep, index: number): string {
  const title = escapeHtml(truncateChars(s.title || s.expected_tool || "step", 80));
  const cap = escapeHtml(s.expected_capability || s.expected_tool || "");
  const stepId = escapeHtml(s.id || String(index));
  let hunksHtml = "";
  if (s.expected_tool === "filesystem.patch" && typeof s.arguments?.diff === "string") {
    const hunks: HunkListItem[] = listHunks(s.arguments.diff);
    if (hunks.length > 0) {
      hunksHtml = `<div class="hunks">${hunks
        .map(
          (h) => `<label class="hunk">
          <input type="checkbox" class="keep-hunk" data-step="${stepId}" data-key="${escapeHtml(h.key)}" checked />
          <span><code>${escapeHtml(h.fileLabel)}</code> +${h.added} −${h.removed} <span class="hdr">${escapeHtml(truncateChars(h.header, 60))}</span></span>
        </label>`,
        )
        .join("")}</div>`;
    }
  }
  return `<div class="step-block">
    <label class="step">
      <input type="checkbox" class="keep" data-idx="${index}" checked />
      <span><strong>${index + 1}.</strong> ${title}${cap ? ` <code>${cap}</code>` : ""}</span>
    </label>
    ${hunksHtml}
  </div>`;
}

function renderHtml(
  webview: vscode.Webview,
  body: string,
  caution: boolean,
  steps: PlanStep[],
  nonce: string,
): string {
  const csp = [
    `default-src 'none'`,
    `style-src ${webview.cspSource} 'unsafe-inline'`,
    `script-src 'nonce-${nonce}'`,
  ].join("; ");
  const stepList =
    steps.length > 0
      ? `<div class="steps">${steps.map((s, i) => stepCheckboxRow(s, i)).join("")}</div>
         <p class="edit-hint">Uncheck steps to drop, or uncheck hunks to skip — then Apply edit.</p>`
      : "";
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta http-equiv="Content-Security-Policy" content="${csp}" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Nomi Plan Review</title>
  <style>
    body {
      font-family: var(--vscode-font-family);
      font-size: var(--vscode-font-size);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      padding: 12px 16px 24px;
      margin: 0;
    }
    .toolbar {
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      align-items: center;
      margin-bottom: 12px;
      position: sticky;
      top: 0;
      background: var(--vscode-editor-background);
      padding: 8px 0;
      z-index: 1;
    }
    button {
      font: inherit;
      padding: 6px 14px;
      border-radius: 2px;
      border: 1px solid var(--vscode-button-border, transparent);
      cursor: pointer;
    }
    button.approve {
      background: var(--vscode-button-background);
      color: var(--vscode-button-foreground);
    }
    button.deny, button.edit {
      background: var(--vscode-button-secondaryBackground);
      color: var(--vscode-button-secondaryForeground);
    }
    .hint {
      font-size: 12px;
      opacity: 0.75;
      margin-left: auto;
    }
    .caution {
      border-left: 3px solid var(--vscode-inputValidation-warningBorder, #cca700);
      padding: 6px 10px;
      margin-bottom: 12px;
      background: var(--vscode-inputValidation-warningBackground, transparent);
      font-size: 12px;
    }
    .steps {
      display: flex;
      flex-direction: column;
      gap: 10px;
      margin-bottom: 8px;
      padding: 8px 10px;
      border: 1px solid var(--vscode-widget-border, #444);
      border-radius: 4px;
    }
    .step, .hunk {
      display: flex;
      align-items: flex-start;
      gap: 8px;
      cursor: pointer;
      font-size: 13px;
    }
    .hunks {
      margin: 4px 0 0 22px;
      display: flex;
      flex-direction: column;
      gap: 4px;
    }
    .hunk { font-size: 12px; opacity: 0.9; }
    .hunk .hdr { opacity: 0.65; font-family: var(--vscode-editor-font-family, monospace); }
    .step code, .hunk code {
      font-size: 11px;
      opacity: 0.8;
    }
    .edit-hint {
      font-size: 12px;
      opacity: 0.75;
      margin: 0 0 12px;
    }
    pre {
      white-space: pre-wrap;
      word-break: break-word;
      font-family: var(--vscode-editor-font-family, monospace);
      font-size: var(--vscode-editor-font-size, 12px);
      line-height: 1.45;
      margin: 0;
    }
  </style>
</head>
<body>
  <div class="toolbar">
    <button class="approve" id="approve">Approve plan</button>
    <button class="edit" id="edit">Apply edit</button>
    <button class="deny" id="deny">Deny</button>
    <span class="hint">Shiki / side-by-side → desktop</span>
  </div>
  ${caution ? `<div class="caution">This plan writes files or runs shell/mutating tools. Review the diff before approving.</div>` : ""}
  ${stepList}
  <pre>${escapeHtml(body)}</pre>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.getElementById('approve').addEventListener('click', () => vscode.postMessage({ type: 'approve' }));
    document.getElementById('deny').addEventListener('click', () => vscode.postMessage({ type: 'deny' }));
    document.getElementById('edit').addEventListener('click', () => {
      const keep = [];
      document.querySelectorAll('input.keep').forEach((el) => {
        if (el.checked) keep.push(Number(el.getAttribute('data-idx')));
      });
      const skippedHunks = {};
      document.querySelectorAll('input.keep-hunk').forEach((el) => {
        if (el.checked) return;
        const step = el.getAttribute('data-step');
        const key = el.getAttribute('data-key');
        if (!step || !key) return;
        if (!skippedHunks[step]) skippedHunks[step] = [];
        skippedHunks[step].push(key);
      });
      vscode.postMessage({ type: 'edit', keep, skippedHunks });
    });
  </script>
</body>
</html>`;
}
