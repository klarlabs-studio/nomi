import * as vscode from "vscode";
import type { NomiClient, Plan, PlanStep, Run } from "./client";
import { listHunks, type HunkListItem } from "./diff_hunks";
import { DIFF_PREVIEW_CSS, renderDiffPreviewHtml } from "./diff_render";
import { openPathFromPlanReview } from "./open_path";
import {
  applyHunkSkips,
  formatPlanReview,
  keepPlanSteps,
  planRequiresCaution,
  summarizeDiff,
  toEditPlanSteps,
  truncateChars,
  truncateLines,
} from "./plan_fmt";

export type PlanReviewCallbacks = {
  onResolved: () => void | Promise<void>;
};

type EditMessage = {
  type?: string;
  keep?: number[];
  path?: string;
  /** Unchecked hunk keys → skip (desktop DiffPreview parity). */
  skippedHunks?: Record<string, string[]>;
};

function preferDarkTheme(): boolean {
  const kind = vscode.window.activeColorTheme.kind;
  return (
    kind === vscode.ColorThemeKind.Dark ||
    kind === vscode.ColorThemeKind.HighContrast
  );
}

/**
 * Plan+diff review panel: drop steps, skip patch hunks, Shiki highlight,
 * and side-by-side DiffPreview chrome (host-side tokens → webview HTML).
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

  const render = async (): Promise<void> => {
    const caution = planRequiresCaution(plan);
    const dark = preferDarkTheme();
    const stepsHtml = await Promise.all(
      (plan?.steps ?? []).map((s, i) => stepCheckboxRow(s, i, dark)),
    );
    const summary = formatPlanReviewSummary(goal, plan);
    const nonce = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    panel.webview.html = renderHtml(panel.webview, summary, caution, stepsHtml, nonce);
  };
  await render();

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
      } else if (msg.type === "openPath") {
        if (typeof msg.path === "string" && msg.path) {
          await openPathFromPlanReview(msg.path);
        }
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
        await render();
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

/** Compact goal + non-diff step meta (diffs render as DiffPreview chrome). */
function formatPlanReviewSummary(goal: string, plan: Plan | null | undefined): string {
  if (!plan) return escapeHtml(formatPlanReview(goal, plan));
  const lines: string[] = ["▶ Plan ready for review"];
  if (goal) lines.push(`  Goal: ${truncateChars(goal, 200)}`);
  return `<pre class="summary">${escapeHtml(lines.join("\n"))}</pre>`;
}

async function stepExtrasHtml(s: PlanStep, preferDark: boolean): Promise<string> {
  const args = s.arguments;
  if (!args) return "";
  const tool = s.expected_tool ?? "";
  if (tool === "filesystem.patch" && typeof args.diff === "string" && args.diff) {
    const truncated =
      args.diff.length > 64_000
        ? `${args.diff.slice(0, 64_000)}\n… (truncated)`
        : args.diff;
    return await renderDiffPreviewHtml(truncated, preferDark);
  }
  if (tool === "filesystem.write") {
    const path = typeof args.path === "string" ? args.path : "";
    const content = typeof args.content === "string" ? args.content : "";
    const bits: string[] = [];
    if (path) {
      bits.push(
        `<button type="button" class="file-chip" data-open-path="${escapeHtml(path)}" title="Open in editor">${escapeHtml(path)}</button>`,
      );
    }
    if (content) {
      const lines = truncateLines(content, 20)
        .split("\n")
        .map((l) => escapeHtml(`| ${l}`))
        .join("\n");
      bits.push(`<pre class="step-args">${lines}</pre>`);
    }
    return bits.length > 0 ? `<div class="write-preview">${bits.join("")}</div>` : "";
  }
  if (tool === "command.exec") {
    const cmd =
      typeof args.command === "string"
        ? args.command
        : typeof args.input === "string"
          ? args.input
          : "";
    return cmd
      ? `<pre class="step-args">${escapeHtml(`$ ${truncateChars(cmd, 200)}`)}</pre>`
      : "";
  }
  if (tool === "filesystem.read" && typeof args.path === "string") {
    return `<pre class="step-args">${escapeHtml(`read: ${args.path}`)}</pre>`;
  }
  return "";
}

async function stepCheckboxRow(
  s: PlanStep,
  index: number,
  preferDark: boolean,
): Promise<string> {
  const title = escapeHtml(truncateChars(s.title || s.expected_tool || "step", 80));
  const cap = escapeHtml(s.expected_capability || s.expected_tool || "");
  const stepId = escapeHtml(s.id || String(index));
  const desc = s.description
    ? `<div class="meta">${escapeHtml(truncateChars(s.description, 200))}</div>`
    : "";
  const why = s.why
    ? `<div class="meta why">why: ${escapeHtml(truncateChars(s.why, 160))}</div>`
    : "";
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
  const extras = await stepExtrasHtml(s, preferDark);
  // summarizeDiff keeps a one-line badge when chrome is empty (degenerate diff)
  let badge = "";
  if (
    s.expected_tool === "filesystem.patch" &&
    typeof s.arguments?.diff === "string" &&
    !extras.includes("diff-preview")
  ) {
    const { added, removed, files } = summarizeDiff(s.arguments.diff);
    badge = `<div class="meta">diff: +${added} −${removed}${files.length ? ` in ${escapeHtml(files.join(", "))}` : ""}</div>`;
  }
  return `<div class="step-block">
    <label class="step">
      <input type="checkbox" class="keep" data-idx="${index}" checked />
      <span><strong>${index + 1}.</strong> ${title}${cap ? ` <code>${cap}</code>` : ""}</span>
    </label>
    ${desc}${why}${badge}
    ${hunksHtml}
    ${extras}
  </div>`;
}

function renderHtml(
  webview: vscode.Webview,
  summaryHtml: string,
  caution: boolean,
  stepsHtml: string[],
  nonce: string,
): string {
  const csp = [
    `default-src 'none'`,
    `style-src ${webview.cspSource} 'unsafe-inline'`,
    `script-src 'nonce-${nonce}'`,
  ].join("; ");
  const stepList =
    stepsHtml.length > 0
      ? `<div class="steps">${stepsHtml.join("")}</div>
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
    button.deny, button.edit, button.toggle-view {
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
    .meta {
      margin: 2px 0 0 28px;
      font-size: 12px;
      opacity: 0.8;
    }
    .meta.why { opacity: 0.7; font-style: italic; }
    .edit-hint {
      font-size: 12px;
      opacity: 0.75;
      margin: 0 0 12px;
    }
    pre.summary, pre.step-args {
      white-space: pre-wrap;
      word-break: break-word;
      font-family: var(--vscode-editor-font-family, monospace);
      font-size: var(--vscode-editor-font-size, 12px);
      line-height: 1.45;
      margin: 0 0 12px;
    }
    pre.step-args {
      margin: 4px 0 0 22px;
      opacity: 0.9;
    }
    .write-preview {
      margin: 4px 0 0 22px;
    }
    .write-preview .file-chip { margin-bottom: 4px; }
    ${DIFF_PREVIEW_CSS}
  </style>
</head>
<body>
  <div class="toolbar">
    <button class="approve" id="approve">Approve plan</button>
    <button class="edit" id="edit">Apply edit</button>
    <button class="deny" id="deny">Deny</button>
    <button class="toggle-view" id="toggle-view" title="Toggle unified / side-by-side">Side-by-side</button>
    <span class="hint" id="view-hint">Unified view</span>
  </div>
  ${caution ? `<div class="caution">This plan writes files or runs shell/mutating tools. Review the diff before approving.</div>` : ""}
  ${summaryHtml}
  ${stepList}
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    const state = vscode.getState() || { split: false };
    function applyView() {
      document.body.classList.toggle('diff-split', !!state.split);
      const btn = document.getElementById('toggle-view');
      const hint = document.getElementById('view-hint');
      if (btn) btn.textContent = state.split ? 'Unified' : 'Side-by-side';
      if (hint) hint.textContent = state.split ? 'Side-by-side view' : 'Unified view';
    }
    applyView();
    document.getElementById('toggle-view').addEventListener('click', () => {
      state.split = !state.split;
      vscode.setState(state);
      applyView();
    });
    document.getElementById('approve').addEventListener('click', () => vscode.postMessage({ type: 'approve' }));
    document.getElementById('deny').addEventListener('click', () => vscode.postMessage({ type: 'deny' }));
    document.body.addEventListener('click', (ev) => {
      const t = ev.target;
      if (!(t instanceof Element)) return;
      const btn = t.closest('[data-open-path]');
      if (!btn) return;
      const p = btn.getAttribute('data-open-path');
      if (p) vscode.postMessage({ type: 'openPath', path: p });
    });
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
