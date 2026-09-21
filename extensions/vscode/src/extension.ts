import * as vscode from "vscode";
import {
  formatApprovalToastMessage,
  parseApprovalRequested,
  shouldShowApprovalToast,
} from "./approval_toast";
import { shouldAutoOpenPlanReview } from "./auto_open_plan";
import { isCancelableStatus, preferCancelCandidates } from "./cancel_run";
import { NomiClient, pendingCount, type Approval, type PendingSnapshot, type Run } from "./client";
import { discoverConnection } from "./discovery";
import { buildEditorContext, type EditorContextPayload } from "./editor_context";
import { isBadgeEvent, type NomiStreamEvent } from "./badge_events";
import { NomiEventStream } from "./event_stream";
import { warmHighlighter } from "./highlighter";
import { isPausableStatus, isPausedStatus } from "./pause_run";
import { openPlanReview } from "./plan_review";
import {
  isProgressEvent,
  shouldRevealProgress,
  StepProgressFormatter,
} from "./step_progress";
import {
  formatStatusBarText,
  statusBarCommand,
  updateLiveStep,
  type LiveStep,
} from "./status_bar";

let statusItem: vscode.StatusBarItem | undefined;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let eventStream: NomiEventStream | undefined;
let refreshDebounce: ReturnType<typeof setTimeout> | undefined;
let lastSnapshot: PendingSnapshot = { approvals: [], plans: [] };
let liveConnected = false;
let progressChannel: vscode.OutputChannel | undefined;
const progressFormatter = new StepProgressFormatter();
const trackedRuns = new Set<string>();
/** Ambient “what’s running” for the status bar (tracked runs only). */
let liveStep: LiveStep | null = null;
/** Runs whose Plan Review panel was already auto-opened this session. */
const autoOpenedPlans = new Set<string>();
const autoOpenInflight = new Set<string>();
/** Approval ids already toasted this session (avoid duplicate banners). */
const toastedApprovals = new Set<string>();
const toastInflight = new Set<string>();

function trackRun(runId: string): void {
  if (runId) trackedRuns.add(runId);
}

function appendProgress(line: string, reveal: boolean): void {
  if (!progressChannel) return;
  progressChannel.appendLine(line);
  if (reveal) {
    progressChannel.show(true); // preserveFocus
  }
}

async function maybeAutoOpenPlanReview(ev: NomiStreamEvent): Promise<void> {
  const cfg = vscode.workspace.getConfiguration("nomi");
  const enabled = cfg.get<boolean>("autoOpenPlanReview") ?? true;
  if (!shouldAutoOpenPlanReview(ev, trackedRuns, enabled, autoOpenedPlans)) {
    return;
  }
  const runId = ev.run_id!;
  if (autoOpenInflight.has(runId)) return;
  autoOpenedPlans.add(runId);
  autoOpenInflight.add(runId);
  try {
    const client = buildClient();
    const detail = await client.getRun(runId);
    if (detail.run.status !== "plan_review") return;
    appendProgress(`▶ [${runId.slice(0, 8)}] opening Plan Review`, true);
    await openPlanReview(client, detail.run, {
      onResolved: () => {
        trackRun(runId);
        return refreshBadge(true);
      },
    });
    await refreshBadge(true);
  } catch (err) {
    autoOpenedPlans.delete(runId);
    vscode.window.showErrorMessage(
      `Nomi: auto Plan Review failed — ${err instanceof Error ? err.message : String(err)}`,
    );
  } finally {
    autoOpenInflight.delete(runId);
  }
}

async function maybeApprovalToast(ev: NomiStreamEvent): Promise<void> {
  const info = parseApprovalRequested(ev);
  if (!info) return;
  const cfg = vscode.workspace.getConfiguration("nomi");
  const enabled = cfg.get<boolean>("approvalToast") ?? true;
  if (!shouldShowApprovalToast(enabled, info.approvalId, toastedApprovals)) {
    return;
  }
  if (toastInflight.has(info.approvalId)) return;
  toastedApprovals.add(info.approvalId);
  toastInflight.add(info.approvalId);
  try {
    const runNote = info.runId ? info.runId.slice(0, 8) : "?";
    appendProgress(`⏸ [${runNote}] approval: ${info.capability}`, true);
    const choice = await vscode.window.showInformationMessage(
      formatApprovalToastMessage(info),
      "Approve",
      "Deny",
    );
    if (!choice) return;
    const client = buildClient();
    await client.resolveApproval(info.approvalId, choice === "Approve");
    vscode.window.showInformationMessage(
      choice === "Approve"
        ? `Nomi: approved ${info.capability}`
        : `Nomi: denied ${info.capability}`,
    );
    await refreshBadge(true);
  } catch (err) {
    toastedApprovals.delete(info.approvalId);
    vscode.window.showErrorMessage(
      `Nomi: ${err instanceof Error ? err.message : String(err)}`,
    );
  } finally {
    toastInflight.delete(info.approvalId);
  }
}

type PendingItem =
  | { itemKind: "approval"; label: string; description: string; approval: Approval }
  | { itemKind: "plan"; label: string; description: string; run: Run };

function buildClient(): NomiClient {
  const cfg = vscode.workspace.getConfiguration("nomi");
  const discovered = discoverConnection({
    apiUrl: cfg.get<string>("apiUrl") || undefined,
    token: cfg.get<string>("token") || undefined,
    dataDir: cfg.get<string>("dataDir") || undefined,
  });
  return new NomiClient(discovered.url, discovered.token);
}

function applyStatusBar(clientUrl?: string): void {
  if (!statusItem) return;
  const n = pendingCount(lastSnapshot);
  statusItem.text = formatStatusBarText(n, liveStep);
  statusItem.command = statusBarCommand(n, liveStep);
  const conn = liveConnected ? " · live" : " · polling";
  if (liveStep?.kind === "plan") {
    statusItem.tooltip = `Plan ready for review (${liveStep.runId.slice(0, 8)}) — click to open${conn}`;
    statusItem.backgroundColor = new vscode.ThemeColor("statusBarItem.warningBackground");
  } else if (n > 0) {
    statusItem.tooltip = `${lastSnapshot.approvals.length} tool approval(s), ${lastSnapshot.plans.length} plan(s) awaiting review${conn}`;
    statusItem.backgroundColor = new vscode.ThemeColor("statusBarItem.warningBackground");
  } else if (liveStep?.kind === "paused") {
    statusItem.tooltip = `Paused (${liveStep.runId.slice(0, 8)}) — click to resume${conn}`;
    statusItem.backgroundColor = undefined;
  } else if (liveStep) {
    statusItem.tooltip = `Running: ${liveStep.title} (${liveStep.runId.slice(0, 8)})${conn}`;
    statusItem.backgroundColor = undefined;
  } else {
    statusItem.tooltip = clientUrl
      ? `Connected to ${clientUrl} — no pending reviews${conn}`
      : `Nomi${conn}`;
    statusItem.backgroundColor = undefined;
  }
}

async function refreshBadge(silent = false): Promise<void> {
  try {
    const client = buildClient();
    lastSnapshot = await client.snapshot();
    applyStatusBar(client.url);
  } catch (err) {
    if (statusItem) {
      statusItem.text = "$(shield) Nomi $(warning)";
      statusItem.tooltip = err instanceof Error ? err.message : String(err);
      statusItem.backgroundColor = undefined;
    }
    if (!silent) {
      vscode.window.showErrorMessage(
        `Nomi: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }
}

function scheduleBadgeRefresh(): void {
  if (refreshDebounce) clearTimeout(refreshDebounce);
  refreshDebounce = setTimeout(() => void refreshBadge(true), 150);
}

function startEventStream(): void {
  eventStream?.dispose();
  eventStream = undefined;
  liveConnected = false;
  try {
    const cfg = vscode.workspace.getConfiguration("nomi");
    const discovered = discoverConnection({
      apiUrl: cfg.get<string>("apiUrl") || undefined,
      token: cfg.get<string>("token") || undefined,
      dataDir: cfg.get<string>("dataDir") || undefined,
    });
    eventStream = new NomiEventStream(discovered.url, discovered.token, {
      onEvent: (ev) => {
        if (isBadgeEvent(ev.type)) scheduleBadgeRefresh();
        if (isProgressEvent(ev.type)) {
          const line = progressFormatter.format(ev);
          if (line) {
            appendProgress(line, shouldRevealProgress(ev, trackedRuns));
          }
          liveStep = updateLiveStep(
            ev,
            trackedRuns,
            liveStep,
            progressFormatter.titleSnapshot(),
          );
          applyStatusBar();
          if (
            ev.run_id &&
            (ev.type === "run.completed" ||
              ev.type === "run.failed" ||
              ev.type === "run.cancelled")
          ) {
            trackedRuns.delete(ev.run_id);
            autoOpenedPlans.delete(ev.run_id);
          }
        }
        if (ev.type === "plan.proposed") {
          void maybeAutoOpenPlanReview(ev);
        }
        if (ev.type === "approval.requested") {
          void maybeApprovalToast(ev);
        }
      },
      onConnect: () => {
        liveConnected = true;
        applyStatusBar();
        void refreshBadge(true);
      },
      onDisconnect: () => {
        liveConnected = false;
        applyStatusBar();
        void refreshBadge(true);
      },
    });
    eventStream.start();
  } catch {
    // Discovery can fail before nomid writes api.endpoint — poll covers it.
  }
}

function toQuickPickItems(snap: PendingSnapshot): (vscode.QuickPickItem & PendingItem)[] {
  const items: (vscode.QuickPickItem & PendingItem)[] = [];
  for (const a of snap.approvals) {
    items.push({
      itemKind: "approval",
      approval: a,
      label: `$(key) ${a.capability}`,
      description: `tool · run ${a.run_id.slice(0, 8)}`,
      detail: a.id,
    });
  }
  for (const r of snap.plans) {
    const goal = r.goal.length > 80 ? `${r.goal.slice(0, 77)}…` : r.goal;
    items.push({
      itemKind: "plan",
      run: r,
      label: `$(list-tree) ${goal || "(no goal)"}`,
      description: `plan · ${r.id.slice(0, 8)}`,
      detail: r.id,
    });
  }
  return items;
}

async function pickPending(placeHolder: string): Promise<PendingItem | undefined> {
  await refreshBadge(true);
  const items = toQuickPickItems(lastSnapshot);
  if (items.length === 0) {
    vscode.window.showInformationMessage("Nomi: nothing pending.");
    return undefined;
  }
  return vscode.window.showQuickPick(items, { placeHolder, matchOnDescription: true });
}

async function approveSelected(): Promise<void> {
  const item = await pickPending("Approve which pending item?");
  if (!item) return;
  const client = buildClient();
  if (item.itemKind === "approval") {
    await client.resolveApproval(item.approval.id, true);
    vscode.window.showInformationMessage(`Nomi: approved ${item.approval.capability}`);
    await refreshBadge(true);
    return;
  }
  // Plans always open the review panel (no blind approve for write/patch).
  trackRun(item.run.id);
  await openPlanReview(client, item.run, {
    onResolved: () => {
      trackRun(item.run.id);
      return refreshBadge(true);
    },
  });
}

async function denySelected(): Promise<void> {
  const item = await pickPending("Deny which pending item?");
  if (!item) return;
  const client = buildClient();
  if (item.itemKind === "approval") {
    await client.resolveApproval(item.approval.id, false);
    vscode.window.showInformationMessage(`Nomi: denied ${item.approval.capability}`);
  } else {
    await client.denyPlan(item.run.id);
    vscode.window.showInformationMessage("Nomi: plan denied (run cancelled)");
  }
  await refreshBadge(true);
}

async function showPending(): Promise<void> {
  const item = await pickPending("Pending Nomi reviews");
  if (!item) return;
  if (item.itemKind === "plan") {
    const client = buildClient();
    trackRun(item.run.id);
    await openPlanReview(client, item.run, {
      onResolved: () => {
        trackRun(item.run.id);
        return refreshBadge(true);
      },
    });
    return;
  }
  const choice = await vscode.window.showQuickPick(
    [
      { label: "Approve", id: "approve" as const },
      { label: "Deny", id: "deny" as const },
    ],
    { placeHolder: "Tool approval" },
  );
  if (!choice) return;
  const client = buildClient();
  if (choice.id === "approve") {
    await client.resolveApproval(item.approval.id, true);
  } else {
    await client.resolveApproval(item.approval.id, false);
  }
  await refreshBadge(true);
}

async function reviewPlanCommand(): Promise<void> {
  await refreshBadge(true);
  const plans = lastSnapshot.plans;
  if (plans.length === 0) {
    vscode.window.showInformationMessage("Nomi: no plans awaiting review.");
    return;
  }
  let run = plans[0]!;
  if (plans.length > 1) {
    const picked = await vscode.window.showQuickPick(
      plans.map((r) => ({
        label: r.goal.length > 80 ? `${r.goal.slice(0, 77)}…` : r.goal || "(no goal)",
        description: r.id.slice(0, 8),
        run: r,
      })),
      { placeHolder: "Review which plan?" },
    );
    if (!picked) return;
    run = picked.run;
  }
  const client = buildClient();
  trackRun(run.id);
  await openPlanReview(client, run, {
    onResolved: () => {
      trackRun(run.id);
      return refreshBadge(true);
    },
  });
}

/** Status-bar plan click: open the live tracked plan without a Quick Pick. */
async function openStatusPlanCommand(): Promise<void> {
  if (liveStep?.kind === "plan" && liveStep.runId) {
    try {
      const client = buildClient();
      const detail = await client.getRun(liveStep.runId);
      if (detail.run.status === "plan_review") {
        trackRun(detail.run.id);
        await openPlanReview(client, detail.run, {
          onResolved: () => {
            trackRun(detail.run.id);
            liveStep = null;
            return refreshBadge(true);
          },
        });
        return;
      }
    } catch (err) {
      vscode.window.showErrorMessage(
        `Nomi: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    }
  }
  await reviewPlanCommand();
}

async function openStatus(): Promise<void> {
  try {
    const client = buildClient();
    const ok = await client.health();
    const snap = await client.snapshot();
    vscode.window.showInformationMessage(
      `Nomi ${ok ? "reachable" : "unreachable"} at ${client.url} — ${pendingCount(snap)} pending`,
    );
  } catch (err) {
    vscode.window.showErrorMessage(`Nomi: ${err instanceof Error ? err.message : String(err)}`);
  }
}

function gatherEditorContext(): EditorContextPayload | undefined {
  const folders = (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
  const tabs: string[] = [];
  for (const group of vscode.window.tabGroups.all) {
    for (const tab of group.tabs) {
      const input = tab.input;
      if (input instanceof vscode.TabInputText) {
        tabs.push(input.uri.fsPath);
      }
    }
  }
  const ed = vscode.window.activeTextEditor;
  let active:
    | {
        path: string;
        languageId?: string;
        selectionText?: string;
        startLine?: number;
        endLine?: number;
      }
    | undefined;
  if (ed) {
    const sel = ed.selection;
    const text = !sel.isEmpty ? ed.document.getText(sel) : "";
    active = {
      path: ed.document.uri.fsPath,
      languageId: ed.document.languageId,
      selectionText: text || undefined,
      startLine: sel.start.line + 1,
      endLine: sel.end.line + 1,
    };
  }
  return buildEditorContext({ workspaceFolders: folders, openTabs: tabs, active });
}

async function resolveAssistantId(client: NomiClient): Promise<string | undefined> {
  const cfg = vscode.workspace.getConfiguration("nomi");
  const configured = (cfg.get<string>("defaultAssistantId") ?? "").trim();
  if (configured) return configured;
  const assistants = await client.listAssistants();
  if (assistants.length === 0) {
    vscode.window.showErrorMessage("Nomi: no assistants configured. Create one in the desktop app.");
    return undefined;
  }
  if (assistants.length === 1) return assistants[0]!.id;
  const picked = await vscode.window.showQuickPick(
    assistants.map((a) => ({ label: a.name, description: a.id, id: a.id })),
    { placeHolder: "Choose a Nomi assistant" },
  );
  return picked?.id;
}

async function runWithEditorContext(): Promise<void> {
  const ed = vscode.window.activeTextEditor;
  const hasSelection = !!ed && !ed.selection.isEmpty;
  const goal = await vscode.window.showInputBox({
    prompt: hasSelection
      ? "What should Nomi do with the selection?"
      : "What should Nomi do?",
    placeHolder: hasSelection
      ? "e.g. Refactor this to return Result, add tests, explain"
      : "e.g. Refactor the open file to return Result",
    ignoreFocusOut: true,
  });
  if (!goal?.trim()) return;

  try {
    const client = buildClient();
    const assistantId = await resolveAssistantId(client);
    if (!assistantId) return;
    const editorContext = gatherEditorContext();
    const run = await client.createRun(goal.trim(), assistantId, editorContext);
    trackRun(run.id);
    const ctxNote = editorContext
      ? ` (tabs=${editorContext.open_tabs?.length ?? 0}, selection=${editorContext.active?.selection ? "yes" : "no"})`
      : " (no editor context)";
    vscode.window.showInformationMessage(`Nomi: run ${run.id.slice(0, 8)} created${ctxNote}`);
    progressChannel?.appendLine(`▶ [${run.id.slice(0, 8)}] Ask Nomi — ${goal.trim().slice(0, 80)}`);
    progressChannel?.show(true);
    await refreshBadge(true);
  } catch (err) {
    vscode.window.showErrorMessage(`Nomi: ${err instanceof Error ? err.message : String(err)}`);
  }
}

async function cancelRunCommand(): Promise<void> {
  try {
    const client = buildClient();
    const runs = await client.listRuns();
    const cancelable = runs.filter((r) => isCancelableStatus(r.status));
    const candidates = preferCancelCandidates(cancelable, trackedRuns);
    if (candidates.length === 0) {
      vscode.window.showInformationMessage("Nomi: no active runs to cancel.");
      return;
    }
    let run = candidates[0]!;
    if (candidates.length > 1) {
      const picked = await vscode.window.showQuickPick(
        candidates.map((r) => ({
          label: r.goal.length > 80 ? `${r.goal.slice(0, 77)}…` : r.goal || "(no goal)",
          description: `${r.status} · ${r.id.slice(0, 8)}`,
          run: r,
        })),
        { placeHolder: "Cancel which run?" },
      );
      if (!picked) return;
      run = picked.run;
    }
    await client.cancelRun(run.id);
    trackedRuns.delete(run.id);
    const short = run.id.slice(0, 8);
    appendProgress(`✗ [${short}] cancel requested`, true);
    vscode.window.showInformationMessage(`Nomi: cancelled ${short}`);
    await refreshBadge(true);
  } catch (err) {
    vscode.window.showErrorMessage(`Nomi: ${err instanceof Error ? err.message : String(err)}`);
  }
}

async function pickRunByStatus(
  match: (status: string) => boolean,
  emptyMsg: string,
  placeHolder: string,
): Promise<Run | undefined> {
  const client = buildClient();
  const runs = await client.listRuns();
  const matching = runs.filter((r) => match(r.status));
  const candidates = preferCancelCandidates(matching, trackedRuns);
  if (candidates.length === 0) {
    vscode.window.showInformationMessage(emptyMsg);
    return undefined;
  }
  if (candidates.length === 1) return candidates[0];
  const picked = await vscode.window.showQuickPick(
    candidates.map((r) => ({
      label: r.goal.length > 80 ? `${r.goal.slice(0, 77)}…` : r.goal || "(no goal)",
      description: `${r.status} · ${r.id.slice(0, 8)}`,
      run: r,
    })),
    { placeHolder },
  );
  return picked?.run;
}

async function pauseRunCommand(): Promise<void> {
  try {
    const run = await pickRunByStatus(
      isPausableStatus,
      "Nomi: no pausable runs (need executing / awaiting_approval).",
      "Pause which run?",
    );
    if (!run) return;
    const client = buildClient();
    await client.pauseRun(run.id);
    const short = run.id.slice(0, 8);
    appendProgress(`⏸ [${short}] pause requested`, true);
    vscode.window.showInformationMessage(`Nomi: paused ${short}`);
    await refreshBadge(true);
  } catch (err) {
    vscode.window.showErrorMessage(`Nomi: ${err instanceof Error ? err.message : String(err)}`);
  }
}

async function resumeRunCommand(): Promise<void> {
  try {
    const run = await pickRunByStatus(
      isPausedStatus,
      "Nomi: no paused runs to resume.",
      "Resume which run?",
    );
    if (!run) return;
    const client = buildClient();
    await client.resumeRun(run.id);
    trackRun(run.id);
    const short = run.id.slice(0, 8);
    appendProgress(`▶ [${short}] resume requested`, true);
    vscode.window.showInformationMessage(`Nomi: resumed ${short}`);
    await refreshBadge(true);
  } catch (err) {
    vscode.window.showErrorMessage(`Nomi: ${err instanceof Error ? err.message : String(err)}`);
  }
}

export function activate(context: vscode.ExtensionContext): void {
  statusItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 100);
  statusItem.command = "nomi.showPending";
  statusItem.text = "$(shield) Nomi";
  statusItem.show();
  context.subscriptions.push(statusItem);

  progressChannel = vscode.window.createOutputChannel("Nomi");
  context.subscriptions.push(progressChannel);

  context.subscriptions.push(
    vscode.commands.registerCommand("nomi.refresh", () => refreshBadge(false)),
    vscode.commands.registerCommand("nomi.showPending", () => showPending()),
    vscode.commands.registerCommand("nomi.approveSelected", () => approveSelected()),
    vscode.commands.registerCommand("nomi.denySelected", () => denySelected()),
    vscode.commands.registerCommand("nomi.openStatus", () => openStatus()),
    vscode.commands.registerCommand("nomi.runWithEditorContext", () => runWithEditorContext()),
    vscode.commands.registerCommand("nomi.reviewPlan", () => reviewPlanCommand()),
    vscode.commands.registerCommand("nomi.openStatusPlan", () => openStatusPlanCommand()),
    vscode.commands.registerCommand("nomi.cancelRun", () => cancelRunCommand()),
    vscode.commands.registerCommand("nomi.pauseRun", () => pauseRunCommand()),
    vscode.commands.registerCommand("nomi.resumeRun", () => resumeRunCommand()),
    vscode.commands.registerCommand("nomi.showProgress", () => {
      progressChannel?.show(true);
    }),
  );

  warmHighlighter();
  void refreshBadge(true);
  startEventStream();
  const cfg = vscode.workspace.getConfiguration("nomi");
  const interval = Math.max(3000, cfg.get<number>("pollIntervalMs") ?? 15_000);
  pollTimer = setInterval(() => void refreshBadge(true), interval);
  context.subscriptions.push({
    dispose: () => {
      if (pollTimer) clearInterval(pollTimer);
      if (refreshDebounce) clearTimeout(refreshDebounce);
      eventStream?.dispose();
    },
  });
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (
        e.affectsConfiguration("nomi.apiUrl") ||
        e.affectsConfiguration("nomi.token") ||
        e.affectsConfiguration("nomi.dataDir")
      ) {
        startEventStream();
        void refreshBadge(true);
      }
    }),
  );
}

export function deactivate(): void {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = undefined;
  if (refreshDebounce) clearTimeout(refreshDebounce);
  refreshDebounce = undefined;
  eventStream?.dispose();
  eventStream = undefined;
}
