import { approvalCopy, type DangerSignal } from "@/lib/approval-copy";
import type { Plan, Run, StepDefinition } from "@/types/api";

export interface PlanTrayCopy {
  summary: string;
  dangerSignal?: DangerSignal;
  /** Capability of the most-dangerous step, used by tray window gating. */
  capability: string;
}

/** Heuristic: MCP / plugin tool names that look like writes/deletes/execs. */
function isMutatingToolName(name: string): boolean {
  const n = name.toLowerCase();
  return (
    n.includes("write") ||
    n.includes("delete") ||
    n.includes("remove") ||
    n.includes("create") ||
    n.includes("update") ||
    n.includes("patch") ||
    n.includes("put") ||
    n.includes("send") ||
    n.includes("post") ||
    n.includes("exec") ||
    n.includes("run") ||
    n.includes("destroy") ||
    n.includes("drop") ||
    n.includes("insert") ||
    n.includes("mutate")
  );
}

function stepCapability(step: StepDefinition): string {
  return step.expected_capability || step.expected_tool || "";
}

/**
 * Aggregate tray copy for a run sitting in plan_review. Mirrors the
 * per-approval danger rules so tray Approve never skips PlanReviewCard /
 * DiffPreview for write/patch/irreversible plans.
 */
export function planTrayCopy(run: Run, plan?: Plan | null): PlanTrayCopy {
  const steps = plan?.steps ?? [];
  const stepCount = steps.length;
  const goal = (run.goal || "").trim();
  const firstTitle = steps[0]?.title?.trim() || "";
  const summary = goal
    ? goal
    : firstTitle
      ? `${stepCount}-step plan: ${firstTitle}`
      : stepCount > 0
        ? `${stepCount}-step plan`
        : "Review proposed plan";

  let dangerSignal: DangerSignal | undefined;
  let capability = "plan.review";

  for (const step of steps) {
    const cap = stepCapability(step);
    const tool = step.expected_tool || "";
    const copy = approvalCopy(cap || tool, {
      input: step.arguments ?? {},
      tool: tool.startsWith("mcp.")
        ? tool.split(".").slice(2).join(".") || tool
        : tool,
    });

    const writeShaped =
      cap === "filesystem.write" ||
      tool === "filesystem.write" ||
      tool === "filesystem.patch";
    const mutateMcp =
      (cap.startsWith("mcp.") || tool.startsWith("mcp.")) &&
      isMutatingToolName(tool || cap);

    if (writeShaped || mutateMcp || copy.dangerSignal === "irreversible") {
      return {
        summary,
        dangerSignal: "irreversible",
        capability: cap || tool || capability,
      };
    }
    if (!dangerSignal && copy.dangerSignal) {
      dangerSignal = copy.dangerSignal;
      capability = cap || tool || capability;
    }
  }

  return { summary, dangerSignal, capability };
}
