import { useQuery } from "@tanstack/react-query";
import { ArrowRight } from "lucide-react";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { approvalsApi } from "@/lib/api";
import { approvalCopy } from "@/lib/approval-copy";
import { errorMessage } from "@/lib/utils";
import { queryKeys } from "@/lib/query-keys";

interface ApprovalPanelProps {
  // Tells App.tsx to switch to the Chats tab and select the run that
  // owns this approval. ApprovalPanel itself never renders the form;
  // it's a list-only navigation surface so the same approval can't be
  // resolved from two places at once.
  onOpenChat: (runId: string) => void;
}

/**
 * ApprovalPanel lists every approval (pending + recent) but does NOT
 * render the resolve form. Pre-refactor, the same approval was
 * resolvable here AND inline in chat-detail, leading to stale state in
 * the surface the user wasn't looking at. Now the panel is purely a
 * list with a deep-link into the chat where the in-context form lives.
 *
 * Single role="status" summary region (aria-live="polite") is the only
 * announcement for screen readers — no per-card alerts.
 */
export function ApprovalPanel({ onOpenChat }: ApprovalPanelProps) {
  const {
    data,
    error: queryError,
    isLoading,
    refetch,
  } = useQuery({
    queryKey: queryKeys.approvals.list(),
    queryFn: () => approvalsApi.list(),
    refetchInterval: 30_000,
  });

  const approvals = data?.approvals ?? [];
  const pendingApprovals = approvals.filter((a) => a.status === "pending");
  const dangerousPending = pendingApprovals.filter(
    (a) => approvalCopy(a.capability, a.context).dangerSignal === "irreversible",
  );

  if (isLoading && approvals.length === 0) {
    return (
      <div className="p-4 flex items-center justify-center h-full">
        <div className="text-muted-foreground">Loading approvals...</div>
      </div>
    );
  }

  if (queryError) {
    return (
      <div className="p-4 space-y-4">
        <h2 className="text-lg font-semibold">Approval Requests</h2>
        <div className="bg-destructive/10 text-destructive p-4 rounded-md">
          <p className="font-medium">Error loading approvals</p>
          <p className="text-sm mt-1">{errorMessage(queryError)}</p>
          <Button
            variant="outline"
            size="sm"
            onClick={() => refetch()}
            className="mt-2"
          >
            Retry
          </Button>
        </div>
      </div>
    );
  }

  // The summary region is the screen-reader's single source of truth for
  // pending counts. Polite by default; switches to assertive only when an
  // irreversible action is queued so that high-stakes prompts still
  // interrupt — but they don't get spammed on every routine pending.
  const summary =
    pendingApprovals.length === 0
      ? "No approval requests pending."
      : dangerousPending.length > 0
        ? `${pendingApprovals.length} approval${pendingApprovals.length === 1 ? "" : "s"} pending, ${dangerousPending.length} irreversible.`
        : `${pendingApprovals.length} approval${pendingApprovals.length === 1 ? "" : "s"} pending.`;

  return (
    <div className="p-4 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Approval Requests</h2>
        {pendingApprovals.length > 0 && (
          <Badge variant="secondary">{pendingApprovals.length} pending</Badge>
        )}
      </div>

      <div
        role="status"
        aria-live={dangerousPending.length > 0 ? "assertive" : "polite"}
        className="sr-only"
      >
        {summary}
      </div>

      {approvals.length === 0 ? (
        <div className="text-muted-foreground py-8 text-center">
          <p>No approval requests.</p>
          <p className="text-sm mt-1">
            When a step requires confirmation, it will appear here.
          </p>
        </div>
      ) : (
        <div className="space-y-3">
          {approvals.map((approval) => {
            const copy = approvalCopy(approval.capability, approval.context);
            const dangerous = copy.dangerSignal === "irreversible";
            return (
              <Card
                key={approval.id}
                // Semantic tokens (destructive / muted-foreground) so
                // dark mode and high-contrast forks render correctly
                // without per-color overrides.
                className={`border-l-4 ${
                  dangerous && approval.status === "pending"
                    ? "border-l-destructive"
                    : approval.status === "pending"
                      ? "border-l-amber-500 dark:border-l-amber-400"
                      : approval.status === "approved"
                        ? "border-l-emerald-500 dark:border-l-emerald-400"
                        : "border-l-destructive"
                }`}
              >
                <CardHeader className="pb-2">
                  <div className="flex items-center justify-between">
                    <div className="text-sm font-medium">{copy.summary}</div>
                    <Badge
                      variant={
                        approval.status === "pending"
                          ? "secondary"
                          : approval.status === "approved"
                            ? "default"
                            : "destructive"
                      }
                    >
                      {approval.status}
                    </Badge>
                  </div>
                </CardHeader>
                <CardContent className="space-y-2">
                  <div className="text-xs text-muted-foreground space-y-1">
                    <div>Capability: {approval.capability}</div>
                    <div>Chat: {approval.run_id?.slice(0, 8)}...</div>
                    {approval.step_id && (
                      <div>Step: {approval.step_id?.slice(0, 8)}...</div>
                    )}
                  </div>

                  {approval.status === "pending" && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => onOpenChat(approval.run_id)}
                      className="gap-2"
                    >
                      Open in chat
                      <ArrowRight className="w-3.5 h-3.5" />
                    </Button>
                  )}
                </CardContent>
              </Card>
            );
          })}
        </div>
      )}
    </div>
  );
}
