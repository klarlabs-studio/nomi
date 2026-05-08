import { useEffect, useRef, useState } from "react";
import {
  ChevronDown,
  ChevronUp,
  CheckCircle,
  AlertCircle,
  Loader2,
  Sparkles,
  GripVertical,
  Plus,
  X,
  Network,
  List,
} from "lucide-react";
import type { Step, Plan, Approval, StepDefinition } from "@/types/api";
import { labels } from "@/lib/labels";
import { approvalCopy } from "@/lib/approval-copy";
import { PlanGraph } from "./plan-graph";

interface ThinkingBlockProps {
  status: string;
  steps: Step[];
  plan?: Plan;
  /** Display name of the assistant whose run this is. Used in status copy
   *  ("X is thinking...", "X needs your approval") so the user sees the
   *  agent they actually configured, not a generic "Nomi". */
  agentName?: string;
}

export function ThinkingBlock({ status, steps, plan, agentName }: ThinkingBlockProps) {
  const [expanded, setExpanded] = useState(false);

  const who = agentName?.trim() || "Nomi";
  const getStatusLabel = () => {
    switch (status) {
      case "planning":
        return `${who} is thinking...`;
      case "executing":
        return `${who} is working...`;
      case "awaiting_approval":
        return `${who} needs your approval`;
      case "paused":
        return "Paused";
      case "completed":
        return "Done";
      case "failed":
        return "Something went wrong";
      default:
        return "Thinking...";
    }
  };

  const isActive = status === "planning" || status === "executing";

  return (
    <div className="bg-muted/50 rounded-lg p-3 text-sm">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex items-center gap-2 w-full text-left text-muted-foreground hover:text-foreground transition-colors"
      >
        {isActive ? (
          <Loader2 className="w-4 h-4 animate-spin" />
        ) : status === "completed" ? (
          <CheckCircle className="w-4 h-4 text-green-500" />
        ) : status === "failed" ? (
          <AlertCircle className="w-4 h-4 text-red-500" />
        ) : (
          <Loader2 className="w-4 h-4" />
        )}
        <span className="flex-1">{getStatusLabel()}</span>
        {expanded ? (
          <ChevronUp className="w-4 h-4" />
        ) : (
          <ChevronDown className="w-4 h-4" />
        )}
      </button>

      {expanded && (
        <div className="mt-2 space-y-1 pl-6">
          {plan?.steps.map((stepDef) => {
            const executedStep = steps.find(
              (s) => s.step_definition_id === stepDef.id
            );
            return (
              <div
                key={stepDef.id}
                className="flex items-center gap-2 text-xs text-muted-foreground"
              >
                <span className="w-4 text-center">
                  {executedStep?.status === "done" ? (
                    <CheckCircle className="w-3 h-3 text-green-500" />
                  ) : executedStep?.status === "failed" ? (
                    <AlertCircle className="w-3 h-3 text-red-500" />
                  ) : executedStep?.status === "running" ? (
                    <Loader2 className="w-3 h-3 animate-spin" />
                  ) : (
                    <span className="w-3 h-3 rounded-full border border-muted-foreground/30 inline-block" />
                  )}
                </span>
                <span>{stepDef.title}</span>
                {stepDef.description && (
                  <span className="text-muted-foreground/60">
                    — {stepDef.description}
                  </span>
                )}
              </div>
            );
          })}
          {!plan && steps.map((step) => (
            <div
              key={step.id}
              className="flex items-center gap-2 text-xs text-muted-foreground"
            >
              <span className="w-4 text-center">
                {step.status === "done" ? (
                  <CheckCircle className="w-3 h-3 text-green-500" />
                ) : step.status === "failed" ? (
                  <AlertCircle className="w-3 h-3 text-red-500" />
                ) : step.status === "running" ? (
                  <Loader2 className="w-3 h-3 animate-spin" />
                ) : (
                  <span className="w-3 h-3 rounded-full border border-muted-foreground/30 inline-block" />
                )}
              </span>
              <span>{step.title}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

interface ApprovalCardProps {
  approval: Approval;
  onResolve: (approved: boolean, remember: boolean) => void;
  processing?: boolean;
  agentName?: string;
}

// IRREVERSIBLE_CONFIRM_TOKEN is the literal string the user must type into
// the confirmation input before the Approve button unlocks for actions
// flagged as irreversible (rm -rf, mkfs, dd if=, etc.). A forcing function
// per Norman: irreversible operations must require explicit positive
// confirmation, not a passive 2-second timeout that a fast clicker can
// race past.
const IRREVERSIBLE_CONFIRM_TOKEN = "confirm";

export function ApprovalCard({ approval, onResolve, processing, agentName }: ApprovalCardProps) {
  const [showRaw, setShowRaw] = useState(false);
  const [confirmText, setConfirmText] = useState("");
  const [rememberChoice, setRememberChoice] = useState(false);
  const copy = approvalCopy(approval.capability, approval.context);
  const dangerous = copy.dangerSignal === "irreversible";
  const unlockApprove = !dangerous || confirmText.trim().toLowerCase() === IRREVERSIBLE_CONFIRM_TOKEN;

  useEffect(() => {
    if (approval.status !== "pending") {
      setConfirmText("");
    }
  }, [approval.status]);

    return (
    <div className={`rounded-lg p-4 my-2 ${dangerous ? "bg-red-50 border border-red-300" : "bg-amber-50 border border-amber-200"}`}
      role={approval.status === "pending" ? "alert" : undefined}
      aria-live={approval.status === "pending" ? "assertive" : undefined}
    >
      <div className="flex items-start gap-3">
        <AlertCircle className={`w-5 h-5 mt-0.5 ${dangerous ? "text-red-600" : "text-amber-600"}`} />
        <div className="flex-1 space-y-2">
          <p className={`text-sm font-medium ${dangerous ? "text-red-900" : "text-amber-900"}`}>
            {(agentName?.trim() || "Nomi") + " needs your approval"}
          </p>
          <p className={`text-sm ${dangerous ? "text-red-800" : "text-amber-800"}`}>
            {copy.summary}
          </p>
          <div className="text-xs">
            <button
              type="button"
              onClick={() => setShowRaw((v) => !v)}
              className="text-muted-foreground hover:underline"
            >
              {showRaw ? "Hide raw details" : "Show raw details"}
            </button>
          </div>
          {showRaw && approval.context && (
            <div className={`text-xs rounded p-2 ${dangerous ? "text-red-700 bg-red-100/50" : "text-amber-700 bg-amber-100/50"}`}>
              {JSON.stringify(approval.context, null, 2)}
            </div>
          )}
          {approval.status === "pending" && dangerous && (
            <div className="space-y-1">
              <label
                htmlFor={`confirm-${approval.id}`}
                className="block text-xs font-medium text-red-900"
              >
                Type <code className="bg-red-100 px-1 rounded">{IRREVERSIBLE_CONFIRM_TOKEN}</code> to enable Approve
              </label>
              <input
                id={`confirm-${approval.id}`}
                type="text"
                value={confirmText}
                onChange={(e) => setConfirmText(e.target.value)}
                autoComplete="off"
                aria-describedby={`confirm-${approval.id}-help`}
                className="w-full rounded-md border border-red-300 bg-white px-2 py-1 text-sm text-red-900 placeholder:text-red-400 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-red-600"
                placeholder={IRREVERSIBLE_CONFIRM_TOKEN}
              />
              <p id={`confirm-${approval.id}-help`} className="text-[11px] text-red-800/80">
                This action cannot be undone.
              </p>
            </div>
          )}
          {approval.status === "pending" && (
            <label className="inline-flex items-center gap-2 text-xs text-muted-foreground">
              <input
                type="checkbox"
                checked={rememberChoice}
                onChange={(e) => setRememberChoice(e.target.checked)}
              />
              Remember this choice for 24 hours
            </label>
          )}
          <div className="flex gap-2 pt-1">
            <button
              onClick={() => onResolve(true, rememberChoice)}
              disabled={processing || !unlockApprove}
              className={`px-3 py-1.5 text-white text-sm rounded-md disabled:opacity-50 transition-colors ${dangerous ? "bg-red-600 hover:bg-red-700" : "bg-amber-600 hover:bg-amber-700"}`}
            >
              {processing ? "Approving..." : "Approve"}
            </button>
            <button
              onClick={() => onResolve(false, rememberChoice)}
              disabled={processing}
              className={`px-3 py-1.5 bg-white border text-sm rounded-md disabled:opacity-50 transition-colors ${dangerous ? "border-red-300 text-red-800 hover:bg-red-50" : "border-amber-300 text-amber-800 hover:bg-amber-50"}`}
            >
              Deny
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

interface PlanReviewCardProps {
  plan: Plan;
  onApprove: () => void;
  onEdit: (
    steps: {
      id?: string;
      title: string;
      description?: string;
      expected_tool?: string;
      expected_capability?: string;
      depends_on?: string[];
    }[]
  ) => void;
  onFork?: (stepID: string) => void;
  onCancel: () => void;
  approving: boolean;
  editing: boolean;
  cancelling: boolean;
}

/**
 * PlanReviewCard surfaces a plan that the runtime has proposed and is
 * blocking on user review. Renders the StepDefinition list as cards so the
 * user can see what Nomi is about to do before committing. Keyboard flow:
 * the Approve button autofocuses on mount; Enter triggers it; Escape
 * cancels (or exits edit mode); Tab cycles through the three footer actions.
 *
 * This is the missing piece that unblocks the end-to-end "type a goal →
 * see a plan → approve → see a response" flow in the UI without falling
 * back to the REST API.
 */
export function PlanReviewCard({
  plan,
  onApprove,
  onEdit,
  onFork,
  onCancel,
  approving,
  editing,
  cancelling,
}: PlanReviewCardProps) {
  const approveRef = useRef<HTMLButtonElement>(null);
  const editRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const [isEditing, setIsEditing] = useState(false);
  const [showAllSteps, setShowAllSteps] = useState(false);
  const [viewMode, setViewMode] = useState<"list" | "graph">("list");
  const [selectedStepID, setSelectedStepID] = useState<string | undefined>();
  const [draftSteps, setDraftSteps] = useState<StepDefinition[]>([]);

  useEffect(() => {
    approveRef.current?.focus();
  }, []);

  useEffect(() => {
    setDraftSteps(plan.steps ?? []);
    setShowAllSteps((plan.steps ?? []).length <= 3);
  }, [plan]);

  // Escape cancels the run. Captured on the card; scoped to keyboard usage
  // so mouse users aren't surprised by stray keys.
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (
      e.key === "Tab" &&
      !isEditing &&
      !approving &&
      !editing &&
      !cancelling
    ) {
      const controls = [approveRef.current, editRef.current, cancelRef.current].filter(
        (el): el is HTMLButtonElement => !!el,
      );

      const currentIndex = controls.findIndex((el) => el === document.activeElement);
      if (currentIndex >= 0) {
        e.preventDefault();
        const nextIndex = e.shiftKey
          ? (currentIndex - 1 + controls.length) % controls.length
          : (currentIndex + 1) % controls.length;
        controls[nextIndex].focus();
      }
      return;
    }

    if (e.key === "Escape" && !approving && !cancelling) {
      e.preventDefault();
      if (isEditing) {
        setIsEditing(false);
        setDraftSteps(plan.steps ?? []);
        return;
      }
      onCancel();
    }
  };

  const busy = approving || editing || cancelling;
  const steps = plan.steps ?? [];
  const visibleSteps = showAllSteps ? draftSteps : draftSteps.slice(0, 3);

  const updateDraft = (
    index: number,
    key: "title" | "description" | "expected_tool",
    value: string,
  ) => {
    setDraftSteps((prev) => {
      const next = [...prev];
      const existing = next[index];
      if (!existing) {
        return prev;
      }
      next[index] = {
        ...existing,
        [key]: value,
      };
      return next;
    });
  };

  const removeDraftStep = (index: number) => {
    setDraftSteps((prev) => prev.filter((_, i) => i !== index));
  };

  const addDraftStep = () => {
    setDraftSteps((prev) => [
      ...prev,
      {
        id: `draft-${Date.now()}`,
        plan_id: plan.id,
        title: "",
        description: "",
        expected_tool: "",
        expected_capability: "",
        order: prev.length,
        depends_on: prev.length > 0 ? [prev[prev.length - 1].id] : [],
        created_at: new Date().toISOString(),
      },
    ]);
    setShowAllSteps(true);
  };

  const saveEdit = () => {
    const payload = draftSteps
      .map((step) => ({
        id: step.id,
        title: step.title.trim(),
        description: step.description?.trim() || undefined,
        expected_tool: step.expected_tool?.trim() || undefined,
        expected_capability: step.expected_capability,
        depends_on: step.depends_on ?? [],
      }))
      .filter((step) => step.title.length > 0);

    if (payload.length === 0) {
      return;
    }

    onEdit(payload);
    setIsEditing(false);
  };

  return (
    <div
      role="region"
      aria-labelledby="plan-review-title"
      onKeyDown={onKeyDown}
      className="border border-primary/30 bg-primary/5 rounded-lg p-4 space-y-3 outline-none"
      tabIndex={-1}
    >
      <div className="flex items-start gap-2">
        <Sparkles className="w-4 h-4 text-primary mt-0.5 flex-shrink-0" />
        <div className="flex-1 min-w-0">
          <h3 id="plan-review-title" className="text-sm font-semibold">
            Plan ready for review
          </h3>
          <p className="text-xs text-muted-foreground mt-0.5">
            {steps.length} step{steps.length === 1 ? "" : "s"}. Review below; execution starts only after you approve.
          </p>
        </div>
        <div className="flex items-center gap-1 flex-shrink-0">
          <button
            type="button"
            onClick={() => setViewMode("list")}
            className={`p-1 rounded ${viewMode === "list" ? "bg-muted" : "hover:bg-muted/50"}`}
            aria-label="List view"
          >
            <List className="w-3.5 h-3.5" />
          </button>
          <button
            type="button"
            onClick={() => setViewMode("graph")}
            className={`p-1 rounded ${viewMode === "graph" ? "bg-muted" : "hover:bg-muted/50"}`}
            aria-label="Graph view"
          >
            <Network className="w-3.5 h-3.5" />
          </button>
        </div>
      </div>

      {viewMode === "graph" ? (
        <PlanGraph
          steps={visibleSteps}
          selectedStepID={selectedStepID}
          onSelectStep={(stepID) => setSelectedStepID(stepID)}
        />
      ) : (
        <ol className="space-y-2">
          {visibleSteps.map((step, i) => (
            <li
              key={step.id}
              className="bg-background border rounded-md p-3 text-sm"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="flex items-start gap-2 min-w-0">
                  <GripVertical
                    aria-hidden="true"
                    className="w-3.5 h-3.5 mt-0.5 text-muted-foreground/60 flex-shrink-0"
                  />
                  <span className="text-xs text-muted-foreground font-mono mt-0.5 flex-shrink-0">
                    {i + 1}.
                  </span>
                  <div className="min-w-0">
                    {isEditing ? (
                      <div className="space-y-2">
                        <input
                          value={step.title}
                          onChange={(e) => updateDraft(i, "title", e.target.value)}
                          placeholder="Step title"
                          className="w-full rounded border px-2 py-1 text-sm"
                        />
                        <input
                          value={step.description || ""}
                          onChange={(e) => updateDraft(i, "description", e.target.value)}
                          placeholder="Step description"
                          className="w-full rounded border px-2 py-1 text-xs"
                        />
                        <input
                          value={step.expected_tool || ""}
                          onChange={(e) => updateDraft(i, "expected_tool", e.target.value)}
                          placeholder="Expected tool (optional)"
                          className="w-full rounded border px-2 py-1 text-xs"
                        />
                      </div>
                    ) : (
                      <>
                        <div className="font-medium truncate">{step.title}</div>
                        {step.description && (
                          <p className="text-xs text-muted-foreground mt-1">
                            {step.description}
                          </p>
                        )}
                        {step.why && (
                          <p className="text-[11px] text-blue-600 dark:text-blue-400 mt-1 italic">
                            {step.why}
                          </p>
                        )}
                        {!!step.depends_on?.length && (
                          <p className="text-[11px] text-muted-foreground mt-1">
                            Depends on: {step.depends_on.map((d) => d.slice(0, 8)).join(", ")}
                          </p>
                        )}
                      </>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-1">
                  {step.expected_tool && !isEditing && (
                    <span className="text-[10px] bg-muted text-muted-foreground px-2 py-0.5 rounded-full font-mono flex-shrink-0">
                      {step.expected_tool}
                    </span>
                  )}
                  {!isEditing && onFork && (
                    <button
                      type="button"
                      onClick={() => onFork(step.id)}
                      className="text-[10px] px-2 py-0.5 rounded-full border border-muted-foreground/30 text-muted-foreground hover:bg-muted"
                    >
                      Branch here
                    </button>
                  )}
                  {isEditing && (
                    <button
                      type="button"
                      onClick={() => removeDraftStep(i)}
                      className="p-1 rounded hover:bg-muted"
                      aria-label={`Remove step ${i + 1}`}
                    >
                      <X className="w-3.5 h-3.5 text-muted-foreground" />
                    </button>
                  )}
                </div>
              </div>
            </li>
          ))}
        </ol>
      )}

      {draftSteps.length > 3 && !isEditing && (
        <button
          type="button"
          className="text-xs text-primary hover:underline"
          onClick={() => setShowAllSteps((prev) => !prev)}
        >
          {showAllSteps ? "Show fewer steps" : `Show all ${draftSteps.length} steps`}
        </button>
      )}

      {/* Why this plan? — aggregate why fields + preference influence */}
      {!isEditing && visibleSteps.some((s) => s.why) && (
        <details className="text-xs border rounded-md p-2" open>
          <summary className="cursor-pointer font-medium text-muted-foreground hover:text-foreground">
            Why this plan?
          </summary>
          <ul className="mt-1.5 space-y-1 pl-4 list-disc">
            {visibleSteps.filter((s) => s.why).map((step) => (
              <li key={step.id} className="text-muted-foreground">
                <span className="text-foreground font-medium">{step.title}:</span> {step.why}
              </li>
            ))}
          </ul>
        </details>
      )}

      <div className="flex items-center justify-end gap-2 pt-1">
        {isEditing ? (
          <>
            <button
              type="button"
              onClick={addDraftStep}
              disabled={busy}
              className="px-3 py-1.5 border text-sm rounded-md hover:bg-muted disabled:opacity-50 transition-colors inline-flex items-center gap-1"
            >
              <Plus className="w-3.5 h-3.5" />
              Add step
            </button>
            <button
              type="button"
              onClick={() => {
                setDraftSteps(plan.steps ?? []);
                setIsEditing(false);
              }}
              disabled={busy}
              className="px-3 py-1.5 border text-sm rounded-md hover:bg-muted disabled:opacity-50 transition-colors"
            >
              Discard changes
            </button>
            <button
              type="button"
              onClick={saveEdit}
              disabled={busy || draftSteps.every((step) => !step.title.trim())}
              className="px-3 py-1.5 bg-primary text-primary-foreground text-sm rounded-md hover:bg-primary/90 disabled:opacity-50 transition-colors"
            >
              {editing ? "Saving..." : "Save plan"}
            </button>
          </>
        ) : (
          <>
            <button
              ref={cancelRef}
              type="button"
              onClick={onCancel}
              disabled={busy}
              className="px-3 py-1.5 border text-sm rounded-md hover:bg-muted disabled:opacity-50 transition-colors"
            >
              {cancelling ? "Cancelling..." : labels.actions.cancelRun}
            </button>
            <button
              ref={editRef}
              type="button"
              onClick={() => setIsEditing(true)}
              disabled={busy}
              className="px-3 py-1.5 border text-sm rounded-md hover:bg-muted disabled:opacity-50 transition-colors"
            >
              Edit plan
            </button>
            <button
              ref={approveRef}
              type="button"
              onClick={onApprove}
              disabled={busy}
              className="px-3 py-1.5 bg-primary text-primary-foreground text-sm rounded-md hover:bg-primary/90 disabled:opacity-50 transition-colors"
            >
              {approving ? "Approving..." : labels.actions.approveAndRun}
            </button>
          </>
        )}
      </div>
    </div>
  );
}
