import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { approvalsApi, runsApi } from "@/lib/api";
import { queryKeys } from "@/lib/query-keys";
import { errorMessage } from "@/lib/utils";
import type { Run } from "@/types/api";
import type { EditPlanStep } from "./chat-detail";

/**
 * useChatActions bundles every mutation the chat surface needs and
 * exposes them as flat callbacks + pending flags. Keeps the
 * ChatInterface shell focused on state + composition rather than
 * mutation boilerplate.
 */
export function useChatActions({
  selectedChat,
  selectedAssistant,
  setSelectedChat,
  setNewMessage,
}: {
  selectedChat: string | null;
  selectedAssistant: string;
  setSelectedChat: (id: string | null) => void;
  setNewMessage: (msg: string) => void;
}) {
  const qc = useQueryClient();
  const [processingApproval, setProcessingApproval] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const createRun = useMutation({
    mutationFn: (goal: string) =>
      runsApi.create({ goal, assistant_id: selectedAssistant }),
    onSuccess: (run) => {
      setNewMessage("");
      setSelectedChat(run.id);
      // Immediately invalidate the runs list; EventProvider also fires on
      // run.created but that round-trips through SSE — this makes the
      // sidebar update feel instant.
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
    onError: (err) => {
      console.error("Failed to send message:", err);
    },
  });

  const deleteRun = useMutation({
    mutationFn: (chatId: string) => runsApi.delete(chatId),
    // Optimistic update: remove the row from the list immediately.
    onMutate: async (chatId) => {
      await qc.cancelQueries({ queryKey: queryKeys.runs.list() });
      const previous = qc.getQueryData(queryKeys.runs.list());
      qc.setQueryData(
        queryKeys.runs.list(),
        (old: { runs: Run[] } | undefined) =>
          old ? { runs: old.runs.filter((r) => r.id !== chatId) } : old,
      );
      if (selectedChat === chatId) {
        setSelectedChat(null);
      }
      return { previous };
    },
    onError: (err, _chatId, context) => {
      if (context?.previous) {
        qc.setQueryData(queryKeys.runs.list(), context.previous);
      }
      setDeleteError(`Failed to delete chat: ${errorMessage(err)}`);
      setTimeout(() => setDeleteError(null), 5000);
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
  });

  const resolveApproval = useMutation({
    mutationFn: ({
      id,
      approved,
      remember,
    }: {
      id: string;
      approved: boolean;
      remember: boolean;
    }) => approvalsApi.resolve(id, approved, remember),
    onMutate: ({ id }) => setProcessingApproval(id),
    onSettled: () => {
      setProcessingApproval(null);
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.approvals(selectedChat) });
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  });

  const approvePlan = useMutation({
    mutationFn: (chatId: string) => runsApi.approvePlan(chatId),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  });

  const editPlan = useMutation({
    mutationFn: ({ chatId, steps }: { chatId: string; steps: EditPlanStep[] }) =>
      runsApi.editPlan(chatId, steps),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  });

  const cancelPlan = useMutation({
    mutationFn: (chatId: string) => runsApi.cancel(chatId),
    onSuccess: (_data, chatId) => {
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
      qc.invalidateQueries({ queryKey: queryKeys.runs.detail(chatId) });
    },
  });

  const forkRun = useMutation({
    mutationFn: ({ chatId, stepId }: { chatId: string; stepId: string }) =>
      runsApi.fork(chatId, stepId),
    onSuccess: (resp) => {
      if (resp?.run?.id) {
        setSelectedChat(resp.run.id);
      }
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
  });

  const pauseRun = useMutation({
    mutationFn: (chatId: string) => runsApi.pause(chatId),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
  });

  const resumeRun = useMutation({
    mutationFn: (chatId: string) => runsApi.resume(chatId),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
      qc.invalidateQueries({ queryKey: queryKeys.runs.list() });
    },
  });

  return {
    createRun,
    deleteRun,
    resolveApproval,
    approvePlan,
    editPlan,
    cancelPlan,
    forkRun,
    pauseRun,
    resumeRun,
    processingApproval,
    deleteError,
    refreshChat: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  };
}
