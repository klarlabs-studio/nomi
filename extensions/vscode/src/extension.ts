import * as vscode from "vscode";
import { NomiClient, pendingCount, type Approval, type PendingSnapshot, type Run } from "./client";
import { discoverConnection } from "./discovery";
import { buildEditorContext, type EditorContextPayload } from "./editor_context";

let statusItem: vscode.StatusBarItem | undefined;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let lastSnapshot: PendingSnapshot = { approvals: [], plans: [] };

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
      statusItem.tooltip =
        n > 0
          ? `${lastSnapshot.approvals.length} tool approval(s), ${lastSnapshot.plans.length} plan(s) awaiting review`
          : `Connected to ${client.url} — no pending reviews`;
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
  } else {
    await client.approvePlan(item.run.id);
    vscode.window.showInformationMessage("Nomi: plan approved");
  }
  await refreshBadge(true);
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
  const choice = await vscode.window.showQuickPick(
    [
      { label: "Approve", id: "approve" as const },
      { label: "Deny", id: "deny" as const },
    ],
    { placeHolder: item.itemKind === "plan" ? "Plan review" : "Tool approval" },
  );
  if (!choice) return;
  if (choice.id === "approve") {
    const client = buildClient();
    if (item.itemKind === "approval") await client.resolveApproval(item.approval.id, true);
    else await client.approvePlan(item.run.id);
  } else {
    const client = buildClient();
    if (item.itemKind === "approval") await client.resolveApproval(item.approval.id, false);
    else await client.denyPlan(item.run.id);
  }
  await refreshBadge(true);
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
  const goal = await vscode.window.showInputBox({
    prompt: "What should Nomi do?",
    placeHolder: "e.g. Refactor the selection to return Result",
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
      ? ` (tabs=${editorContext.open_tabs.length}, selection=${editorContext.active?.selection ? "yes" : "no"})`
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
  );

  void refreshBadge(true);
  const cfg = vscode.workspace.getConfiguration("nomi");
  const interval = Math.max(3000, cfg.get<number>("pollIntervalMs") ?? 15_000);
  pollTimer = setInterval(() => void refreshBadge(true), interval);
  context.subscriptions.push({
    dispose: () => {
      if (pollTimer) clearInterval(pollTimer);
    },
  });
}

export function deactivate(): void {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = undefined;
}
