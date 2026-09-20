// CLI-parity plan formatting for the VS Code plan-review panel.
// Mirrors cmd/nomi/plan_fmt.go — read-only steps + diffs, no Shiki / hunk edit.

import type { Plan, PlanStep } from "./client";

export const MAX_PLAN_STEPS_PRINTED = 12;
export const MAX_WRITE_CONTENT_LINES = 20;
export const MAX_DIFF_PRINT_CHARS = 8000;

export type DiffSummary = {
  files: string[];
  added: number;
  removed: number;
};

/** Count +/- lines and collect paths from a unified diff (best-effort). */
export function summarizeDiff(diff: string): DiffSummary {
  const files: string[] = [];
  const seen = new Set<string>();
  let added = 0;
  let removed = 0;
  for (const line of diff.split("\n")) {
    if (line.startsWith("+++ ")) {
      let path = line.slice(4).trim();
      if (path.startsWith("b/")) path = path.slice(2);
      if (path && path !== "/dev/null" && !seen.has(path)) {
        seen.add(path);
        files.push(path);
      }
      continue;
    }
    if (line.startsWith("--- ")) continue;
    if (line.startsWith("+++") || line.startsWith("---")) continue;
    if (line.startsWith("@@")) continue;
    if (line.startsWith("+")) added++;
    else if (line.startsWith("-")) removed++;
  }
  return { files, added, removed };
}

export function truncateChars(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…`;
}

export function truncateLines(s: string, max: number): string {
  const lines = s.split("\n");
  if (lines.length <= max) return s;
  return `${lines.slice(0, max).join("\n")}\n…`;
}

function stepCommand(s: PlanStep): string {
  const args = s.arguments;
  if (!args) return "";
  if (typeof args.command === "string") return args.command;
  if (typeof args.input === "string") return args.input;
  return "";
}

function isIrreversibleCommand(cmd: string): boolean {
  const lower = cmd.toLowerCase();
  return (
    lower.includes("rm -rf") ||
    lower.startsWith("rm ") ||
    lower.includes("mkfs") ||
    lower.includes("dd if=")
  );
}

function isMutatingToolName(name: string): boolean {
  const n = name.toLowerCase();
  for (const needle of [
    "write",
    "delete",
    "remove",
    "create",
    "update",
    "patch",
    "put",
    "send",
    "post",
    "exec",
    "run",
    "destroy",
    "drop",
    "insert",
    "mutate",
  ]) {
    if (n.includes(needle)) return true;
  }
  return false;
}

/** Write/patch / irreversible shell / mutate-shaped MCP — match tray/channel gate. */
export function planRequiresCaution(plan: Plan | null | undefined): boolean {
  if (!plan) return false;
  for (const s of plan.steps) {
    const cap = s.expected_capability ?? "";
    const tool = s.expected_tool ?? "";
    if (
      cap === "filesystem.write" ||
      tool === "filesystem.write" ||
      tool === "filesystem.patch"
    ) {
      return true;
    }
    if ((cap === "command.exec" || tool === "command.exec") && isIrreversibleCommand(stepCommand(s))) {
      return true;
    }
    const name = tool || cap;
    if ((cap.startsWith("mcp.") || tool.startsWith("mcp.")) && isMutatingToolName(name)) {
      return true;
    }
  }
  return false;
}

function formatStepArguments(s: PlanStep): string[] {
  const out: string[] = [];
  const args = s.arguments;
  if (!args) return out;
  const tool = s.expected_tool ?? "";
  switch (tool) {
    case "filesystem.patch": {
      const diff = typeof args.diff === "string" ? args.diff : "";
      if (!diff) break;
      const { files, added, removed } = summarizeDiff(diff);
      let line = `diff: +${added} −${removed}`;
      if (files.length > 0) line += ` in ${files.join(", ")}`;
      out.push(line);
      out.push("---");
      out.push(...truncateChars(diff, MAX_DIFF_PRINT_CHARS).split("\n"));
      out.push("---");
      break;
    }
    case "filesystem.write": {
      const path = typeof args.path === "string" ? args.path : "";
      const content = typeof args.content === "string" ? args.content : "";
      if (path) out.push(`write: ${path}`);
      if (content) {
        for (const l of truncateLines(content, MAX_WRITE_CONTENT_LINES).split("\n")) {
          out.push(`| ${l}`);
        }
      }
      break;
    }
    case "filesystem.read": {
      const path = typeof args.path === "string" ? args.path : "";
      if (path) out.push(`read: ${path}`);
      break;
    }
    case "command.exec": {
      const cmd = stepCommand(s);
      if (cmd) out.push(`$ ${truncateChars(cmd, 200)}`);
      break;
    }
    default: {
      const keys = Object.keys(args);
      if (keys.length > 0) out.push(`args: ${keys.join(", ")}`);
    }
  }
  return out;
}

/** Plain-text plan review (CLI layout) for the webview <pre>. */
export function formatPlanReview(goal: string, plan: Plan | null | undefined): string {
  const lines: string[] = [];
  if (!plan) {
    lines.push("▶ plan ready (empty)");
    return lines.join("\n");
  }
  lines.push("▶ Plan ready for review");
  if (goal) lines.push(`  Goal: ${truncateChars(goal, 200)}`);
  lines.push("");

  const limit = Math.min(plan.steps.length, MAX_PLAN_STEPS_PRINTED);
  for (let i = 0; i < limit; i++) {
    const s = plan.steps[i]!;
    let title = s.title || s.expected_tool || "step";
    title = truncateChars(title, 100);
    const cap = s.expected_capability || s.expected_tool || "";
    lines.push(`  ${i + 1}. ${title}${cap ? ` — \`${cap}\`` : ""}`);
    if (s.description) lines.push(`     ${truncateChars(s.description, 200)}`);
    if (s.why) lines.push(`     why: ${truncateChars(s.why, 160)}`);
    for (const argLine of formatStepArguments(s)) {
      lines.push(`     ${argLine}`);
    }
  }
  if (plan.steps.length > MAX_PLAN_STEPS_PRINTED) {
    lines.push(`  (+${plan.steps.length - MAX_PLAN_STEPS_PRINTED} more)`);
  }
  if (planRequiresCaution(plan)) {
    lines.push("");
    lines.push("  ⚠ This plan writes files or runs shell/mutating tools — review carefully.");
  }
  return lines.join("\n");
}
