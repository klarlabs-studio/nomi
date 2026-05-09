import { useCallback, useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, Bot, Pause, Play, RefreshCw, Sparkles } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ApprovalCard, PlanReviewCard, ThinkingBlock } from "@/components/chat-message";
import { pickResponseText } from "@/lib/response-text";
import { runsApi } from "@/lib/api";
import { queryKeys } from "@/lib/query-keys";
import type { Approval, Assistant, RunWithSteps } from "@/types/api";
import { ChatComposer } from "./chat-composer";

function AgentHeader({ name }: { name: string }) {
  return (
    <div className="flex items-center gap-2">
      <div className="w-6 h-6 bg-primary/10 rounded-full flex items-center justify-center">
        <Bot className="w-3.5 h-3.5 text-primary" />
      </div>
      <span className="text-xs font-medium text-muted-foreground">{name}</span>
    </div>
  );
}

export type EditPlanStep = {
  id?: string;
  title: string;
  description?: string;
  expected_tool?: string;
  expected_capability?: string;
  depends_on?: string[];
};

export function ChatDetail({
  chatData,
  assistants,
  pendingApprovals,
  streamingText,
  processingApproval,
  newMessage,
  onNewMessageChange,
  onSend,
  sending,
  selectedAssistant,
  onResolveApproval,
  onApprovePlan,
  onEditPlan,
  onForkRun,
  onCancelPlan,
  approvePlanPending,
  editPlanPending,
  cancelPlanPending,
  onPause,
  onResume,
  pausePending,
  resumePending,
  onRefresh,
}: {
  chatData: RunWithSteps;
  assistants: Assistant[];
  pendingApprovals: Approval[];
  streamingText: string;
  processingApproval: string | null;
  newMessage: string;
  onNewMessageChange: (msg: string) => void;
  onSend: () => void;
  sending: boolean;
  selectedAssistant: string;
  onResolveApproval: (approvalId: string, approved: boolean, remember: boolean) => void;
  onApprovePlan: () => void;
  onEditPlan: (steps: EditPlanStep[]) => void;
  onForkRun: (stepId: string) => void;
  onCancelPlan: () => void;
  approvePlanPending: boolean;
  editPlanPending: boolean;
  cancelPlanPending: boolean;
  onPause: () => void;
  onResume: () => void;
  pausePending: boolean;
  resumePending: boolean;
  onRefresh: () => void;
}) {
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const messagesContainerRef = useRef<HTMLDivElement>(null);
  const prevStepCountRef = useRef(0);
  const prevStatusRef = useRef<string>("");
  const scrollFrameRef = useRef<number | null>(null);
  const scrollTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [isAtBottom, setIsAtBottom] = useState(true);

  const handleScroll = () => {
    const container = messagesContainerRef.current;
    if (!container) return;
    const threshold = 100;
    const distanceFromBottom =
      container.scrollHeight - container.scrollTop - container.clientHeight;
    setIsAtBottom(distanceFromBottom < threshold);
  };

  const scrollToBottom = () => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
    setIsAtBottom(true);
  };

  const debouncedScrollToBottom = useCallback(() => {
    if (scrollTimeoutRef.current) {
      clearTimeout(scrollTimeoutRef.current);
    }
    scrollTimeoutRef.current = setTimeout(() => {
      if (isAtBottom) {
        messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
      }
    }, 100);
  }, [isAtBottom]);

  // Smart scroll: only scroll when new content is added or a terminal
  // transition happens, not on every refetch. Refs gate the effect so
  // it's idempotent when the query data hasn't meaningfully changed.
  useEffect(() => {
    const currentStepCount = chatData.steps?.length || 0;
    const currentStatus = chatData.run.status;

    const shouldScroll =
      (currentStepCount > prevStepCountRef.current ||
        (currentStatus !== prevStatusRef.current &&
          (currentStatus === "completed" ||
            currentStatus === "failed" ||
            prevStatusRef.current === "created"))) &&
      isAtBottom;

    if (shouldScroll) {
      if (scrollFrameRef.current !== null) {
        cancelAnimationFrame(scrollFrameRef.current);
      }
      scrollFrameRef.current = requestAnimationFrame(() => {
        scrollFrameRef.current = null;
        debouncedScrollToBottom();
      });
    }

    prevStepCountRef.current = currentStepCount;
    prevStatusRef.current = currentStatus;
  }, [chatData, isAtBottom, debouncedScrollToBottom]);

  useEffect(() => {
    return () => {
      if (scrollFrameRef.current !== null) {
        cancelAnimationFrame(scrollFrameRef.current);
      }
      if (scrollTimeoutRef.current) {
        clearTimeout(scrollTimeoutRef.current);
      }
    };
  }, []);

  const responseText = pickResponseText(chatData.steps, chatData.plan);
  const assistantName =
    assistants.find((a) => a.id === chatData.run.assistant_id)?.name || "Nomi";
  const status = chatData.run.status;
  const showPauseResume =
    status === "executing" || status === "awaiting_approval" || status === "paused";

  return (
    <>
      <div className="border-b px-4 py-3 flex items-center justify-between flex-shrink-0">
        <div className="min-w-0">
          <h3 className="font-medium truncate">
            {chatData.run.goal && chatData.run.goal.length > 80
              ? chatData.run.goal.slice(0, 80) + "..."
              : chatData.run.goal}
          </h3>
          <p className="text-xs text-muted-foreground">{assistantName}</p>
        </div>
        <div className="flex items-center gap-2 flex-shrink-0">
          {showPauseResume && (
            <Button
              variant="outline"
              size="sm"
              onClick={status === "paused" ? onResume : onPause}
              disabled={pausePending || resumePending}
            >
              {status === "paused" ? (
                <Play className="w-3 h-3 mr-1" />
              ) : (
                <Pause className="w-3 h-3 mr-1" />
              )}
              {status === "paused" ? "Resume" : "Pause"}
            </Button>
          )}
          <Button variant="outline" size="sm" onClick={onRefresh}>
            <RefreshCw className="w-3 h-3 mr-1" />
            Refresh
          </Button>
          <Badge
            variant={
              status === "completed"
                ? "default"
                : status === "failed"
                  ? "destructive"
                  : "secondary"
            }
          >
            {status}
          </Badge>
          {chatData.run.run_parent_id && (
            <Badge variant="outline" className="font-mono text-[10px]">
              branched from {chatData.run.run_parent_id.slice(0, 8)}
            </Badge>
          )}
        </div>
      </div>

      <div
        ref={messagesContainerRef}
        className="flex-1 overflow-y-auto p-4 space-y-4 min-h-0 relative"
        role="log"
        aria-live="polite"
        onScroll={handleScroll}
      >
        <div className="flex justify-end">
          <div className="bg-primary text-primary-foreground rounded-2xl rounded-tr-sm px-4 py-3 max-w-[80%]">
            <p className="text-sm">{chatData.run.goal}</p>
          </div>
        </div>

        {/* Plan Review — blocks until the user approves or cancels */}
        {status === "plan_review" && chatData.plan && (
          <div className="flex justify-start">
            <div className="max-w-[80%] w-full space-y-2">
              <AgentHeader name={assistantName} />
              <PlanReviewCard
                plan={chatData.plan}
                onApprove={onApprovePlan}
                onEdit={onEditPlan}
                onFork={onForkRun}
                onCancel={onCancelPlan}
                approving={approvePlanPending}
                editing={editPlanPending}
                cancelling={cancelPlanPending}
              />
            </div>
          </div>
        )}

        {/* Thinking / Status Block. Hidden during plan_review so the
            PlanReviewCard is the only actionable surface. */}
        {status !== "created" && status !== "plan_review" && !responseText && (
          <div className="flex justify-start">
            <div className="max-w-[80%] space-y-2">
              <AgentHeader name={assistantName} />
              <ThinkingBlock
                status={status}
                steps={chatData.steps}
                plan={chatData.plan}
                agentName={assistantName}
              />
            </div>
          </div>
        )}

        {/* Live streaming buffer for the currently-running step.
            Hidden when the step has produced no tokens yet (so we
            don't render an empty bubble) and once the persisted
            output supersedes the live text via responseText. */}
        {streamingText && !responseText && (
          <div className="flex justify-start">
            <div className="max-w-[80%] space-y-2">
              <AgentHeader name={assistantName} />
              <div
                className="bg-muted rounded-2xl rounded-tl-sm px-4 py-3"
                aria-live="polite"
                aria-atomic="false"
              >
                <p className="text-sm whitespace-pre-wrap">
                  {streamingText}
                  <span
                    className="ml-0.5 inline-block w-1.5 h-3 bg-foreground/60 align-baseline animate-pulse"
                    aria-hidden="true"
                  />
                </p>
              </div>
            </div>
          </div>
        )}

        {pendingApprovals.map((approval) => (
          <div key={approval.id} className="flex justify-start">
            <div className="max-w-[80%]">
              <ApprovalCard
                approval={approval}
                onResolve={(approved, remember) =>
                  onResolveApproval(approval.id, approved, remember)
                }
                processing={processingApproval === approval.id}
                agentName={assistantName}
              />
            </div>
          </div>
        ))}

        {responseText && (
          <div className="flex justify-start">
            <div className="max-w-[80%] space-y-2">
              <AgentHeader name={assistantName} />
              <div className="bg-muted rounded-2xl rounded-tl-sm px-4 py-3">
                <p className="text-sm whitespace-pre-wrap">{responseText}</p>
              </div>
            </div>
          </div>
        )}

        {status === "failed" && chatData.steps.some((s) => s.error) && (
          <div className="flex justify-start">
            <div className="max-w-[80%] bg-destructive/10 border border-destructive/40 rounded-lg px-4 py-3 space-y-2">
              <p className="text-sm text-destructive">
                {chatData.steps.find((s) => s.error)?.error}
              </p>
              <ReplanCTA runID={chatData.run.id} />
            </div>
          </div>
        )}

        <div ref={messagesEndRef} />

        {!isAtBottom && (
          <Button
            variant="secondary"
            size="sm"
            className="absolute bottom-4 right-4 rounded-full shadow-lg z-10"
            onClick={scrollToBottom}
          >
            <ArrowDown className="w-4 h-4" />
          </Button>
        )}
      </div>

      <ChatComposer
        newMessage={newMessage}
        onNewMessageChange={onNewMessageChange}
        onSend={onSend}
        sending={sending}
        disabled={status === "plan_review"}
        selectedAssistant={selectedAssistant}
        status={status}
      />
    </>
  );
}

// ReplanCTA wraps a one-click "Fix this with the agent" call to
// /runs/:id/replan. Server-side budget (MaxReplansPerRun) bounds how
// many times we can ask, so the button can stay visible across
// retries; the API will refuse once the budget is gone.
function ReplanCTA({ runID }: { runID: string }) {
  const qc = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => runsApi.replan(runID),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: queryKeys.runs.detail(runID) });
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
  });
  return (
    <div className="flex items-center gap-2">
      <Button
        size="sm"
        variant="outline"
        onClick={() => mutation.mutate()}
        disabled={mutation.isPending}
        className="gap-2"
      >
        <Sparkles className="w-3.5 h-3.5" />
        {mutation.isPending ? "Replanning..." : "Fix this with the agent"}
      </Button>
      {mutation.error && (
        <span className="text-xs text-destructive">
          {mutation.error instanceof Error ? mutation.error.message : "Replan failed"}
        </span>
      )}
    </div>
  );
}
