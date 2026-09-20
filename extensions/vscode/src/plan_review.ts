import * as vscode from "vscode";
import type { NomiClient, Plan, Run } from "./client";
import { formatPlanReview, planRequiresCaution } from "./plan_fmt";

export type PlanReviewCallbacks = {
  onResolved: () => void | Promise<void>;
};

/**
 * Read-only plan+diff review panel (CLI --review parity).
 * Approve / Deny call existing endpoints; hunk edit stays on desktop.
 */
export async function openPlanReview(
  client: NomiClient,
  run: Run,
  callbacks: PlanReviewCallbacks,
): Promise<void> {
  const detail = await client.getRun(run.id);
  const plan: Plan | null = detail.plan;
  const goal = detail.run?.goal ?? run.goal;
  const caution = planRequiresCaution(plan);
  const body = formatPlanReview(goal, plan);

  const panel = vscode.window.createWebviewPanel(
    "nomiPlanReview",
    `Nomi Plan · ${run.id.slice(0, 8)}`,
    vscode.ViewColumn.Beside,
    { enableScripts: true, retainContextWhenHidden: true },
  );

  const nonce = String(Date.now());
  panel.webview.html = renderHtml(panel.webview, body, caution, nonce);

  const sub = panel.webview.onDidReceiveMessage(async (msg: { type?: string }) => {
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

function renderHtml(
  webview: vscode.Webview,
  body: string,
  caution: boolean,
  nonce: string,
): string {
  const csp = [
    `default-src 'none'`,
    `style-src ${webview.cspSource} 'unsafe-inline'`,
    `script-src 'nonce-${nonce}'`,
  ].join("; ");
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
    button.deny {
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
    <button class="deny" id="deny">Deny</button>
    <span class="hint">Hunk skip / plan edit → Nomi desktop app</span>
  </div>
  ${caution ? `<div class="caution">This plan writes files or runs shell/mutating tools. Review the diff before approving.</div>` : ""}
  <pre>${escapeHtml(body)}</pre>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.getElementById('approve').addEventListener('click', () => vscode.postMessage({ type: 'approve' }));
    document.getElementById('deny').addEventListener('click', () => vscode.postMessage({ type: 'deny' }));
  </script>
</body>
</html>`;
}
