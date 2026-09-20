import * as vscode from "vscode";
import { NomiClient, pendingCount, type Approval, type PendingSnapshot, type Run } from "./client";
import { discoverConnection } from "./discovery";
import { buildEditorContext, type EditorContextPayload } from "./editor_context";
import { isBadgeEvent } from "./badge_events";
import { NomiEventStream } from "./event_stream";
import { openPlanReview } from "./plan_review";

let statusItem: vscode.StatusBarItem | undefined;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let eventStream: NomiEventStream | undefined;
let refreshDebounce: ReturnType<typeof setTimeout> | undefined;
let lastSnapshot: PendingSnapshot = { approvals: [], plans: [] };
let liveConnected = false;

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

async function refreshBadge(silent = false): Promise<void> {
  try {
    const client = buildClient();
    lastSnapshot = await client.snapshot();
    const n = pendingCount(lastSnapshot);
    if (statusItem) {
      statusItem.text = n > 0 ? `$(shield) Nomi ${n}` : "$(shield) Nomi";
      const live = liveConnected ? " · live" : " · polling";
      statusItem.tooltip =
        n > 0
          ? `${lastSnapshot.approvals.length} tool approval(s), ${lastSnapshot.plans.length} plan(s) awaiting review${live}`
          : `Connected to ${client.url} — no pending reviews${live}`;
      statusItem.backgroundColor =
        n > 0 ? new vscode.ThemeColor("statusBarItem.warningBackground") : undefined;
    }
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
      },
      onConnect: () => {
        liveConnected = true;
        void refreshBadge(true);
      },
      onDisconnect: () => {
        liveConnected = false;
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
  await openPlanReview(client, item.run, { onResolved: () => refreshBadge(true) });
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
    await openPlanReview(client, item.run, { onResolved: () => refreshBadge(true) });
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
  await openPlanReview(client, run, { onResolved: () => refreshBadge(true) });
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
    const ctxNote = editorContext
      ? ` (tabs=${editorContext.open_tabs?.length ?? 0}, selection=${editorContext.active?.selection ? "yes" : "no"})`
      : " (no editor context)";
    vscode.window.showInformationMessage(`Nomi: run ${run.id.slice(0, 8)} created${ctxNote}`);
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

  context.subscriptions.push(
    vscode.commands.registerCommand("nomi.refresh", () => refreshBadge(false)),
    vscode.commands.registerCommand("nomi.showPending", () => showPending()),
    vscode.commands.registerCommand("nomi.approveSelected", () => approveSelected()),
    vscode.commands.registerCommand("nomi.denySelected", () => denySelected()),
    vscode.commands.registerCommand("nomi.openStatus", () => openStatus()),
    vscode.commands.registerCommand("nomi.runWithEditorContext", () => runWithEditorContext()),
    vscode.commands.registerCommand("nomi.reviewPlan", () => reviewPlanCommand()),
  );

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
