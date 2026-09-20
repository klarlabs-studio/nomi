import * as vscode from "vscode";
import type { NomiClient, Plan, PlanStep, Run } from "./client";
import {
  formatPlanReview,
  keepPlanSteps,
  planRequiresCaution,
  toEditPlanSteps,
  truncateChars,
} from "./plan_fmt";

export type PlanReviewCallbacks = {
  onResolved: () => void | Promise<void>;
};

/**
 * Plan+diff review panel with optional step drop (CLI --review [E]dit parity).
 * Hunk skip / Shiki stay on the desktop DiffPreview.
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

  const sub = panel.webview.onDidReceiveMessage(
    async (msg: { type?: string; keep?: number[] }) => {
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
          if (keep.length === plan.steps.length) {
            vscode.window.showInformationMessage("Nomi: no steps dropped");
            return;
          }
          const edited = keepPlanSteps(plan, keep);
          await client.editPlan(run.id, toEditPlanSteps(edited));
          detail = await client.getRun(run.id);
          plan = detail.plan;
          goal = detail.run?.goal ?? goal;
          vscode.window.showInformationMessage(
            `Nomi: plan updated — ${edited.steps.length} step(s) kept`,
          );
          render();
        }
      } catch (err) {
        vscode.window.showErrorMessage(
          `Nomi: ${err instanceof Error ? err.message : String(err)}`,
        );
      }
    },
  );
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
  return `<label class="step">
    <input type="checkbox" class="keep" data-idx="${index}" checked />
    <span><strong>${index + 1}.</strong> ${title}${cap ? ` <code>${cap}</code>` : ""}</span>
  </label>`;
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
         <p class="edit-hint">Uncheck steps to drop, then Apply edit (same as CLI <code>--review</code> [E]dit).</p>`
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
      gap: 6px;
      margin-bottom: 8px;
      padding: 8px 10px;
      border: 1px solid var(--vscode-widget-border, #444);
      border-radius: 4px;
    }
    .step {
      display: flex;
      align-items: flex-start;
      gap: 8px;
      cursor: pointer;
      font-size: 13px;
    }
    .step code {
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
    <span class="hint">Hunk skip → Nomi desktop app</span>
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
      vscode.postMessage({ type: 'edit', keep });
    });
  </script>
</body>
</html>`;
}
