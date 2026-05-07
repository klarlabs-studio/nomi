import { useEffect, useCallback, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { runsApi, assistantsApi, approvalsApi, pluginsApi } from "@/lib/api";
import type { Run, Assistant, Plugin } from "@/types/api";
import { ThinkingBlock, ApprovalCard, PlanReviewCard } from "@/components/chat-message";
import { pickResponseText } from "@/lib/response-text";
import { useStepStream } from "@/lib/streaming";
import { OutcomeConnectorPicker } from "@/components/onboarding/outcome-connectors";
import { queryKeys } from "@/lib/query-keys";
import { errorMessage } from "@/lib/utils";
import { Send, Plus, Bot, Loader2, RefreshCw, Trash2, Pause, Play, Plug, ArrowDown } from "lucide-react";

interface ChatItem {
  id: string;
  title: string;
  status: string;
  assistantName: string;
  createdAt: string;
  // Channel the run originated from (ADR 0001 §8). Drives the small
  // channel badge rendered next to the title so users can see whether
  // a run came from Telegram, Desktop, etc.
  source?: string;
  // Nonempty when the run is part of a threaded Conversation. Sibling
  // runs sharing a conversation_id form a multi-turn thread in the
  // sidebar (see groupByConversation below).
  conversationID?: string;
  runParentID?: string;
}

function ChatSidebarItem({
  chat,
  active,
  onClick,
  onDelete,
}: {
  chat: ChatItem;
  active: boolean;
  onClick: () => void;
  onDelete: () => void;
}) {
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const confirmTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Cancel a pending auto-hide timer when the row unmounts (chat deleted,
  // sidebar collapsed). Without this, setConfirmingDelete fires on an
  // unmounted component and React logs a warning in dev / leaks the
  // closure's reference to the row in production.
  useEffect(() => {
    return () => {
      if (confirmTimerRef.current) {
        clearTimeout(confirmTimerRef.current);
        confirmTimerRef.current = null;
      }
    };
  }, []);

  const statusColors: Record<string, string> = {
    created: "bg-gray-300",
    planning: "bg-blue-400 animate-pulse",
    plan_review: "bg-amber-400",
    executing: "bg-blue-500 animate-pulse",
    awaiting_approval: "bg-amber-500",
    paused: "bg-yellow-500",
    completed: "bg-green-500",
    failed: "bg-red-500",
    cancelled: "bg-gray-400",
  };

  const handleDeleteClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (!confirmingDelete) {
      setConfirmingDelete(true);
      // Auto-hide confirmation after 3 seconds. Cancelled in the
      // unmount cleanup so a rapid delete-then-unmount doesn't leak the
      // setState into a destroyed component.
      if (confirmTimerRef.current) clearTimeout(confirmTimerRef.current);
      confirmTimerRef.current = setTimeout(() => {
        confirmTimerRef.current = null;
        setConfirmingDelete(false);
      }, 3000);
    } else {
      if (confirmTimerRef.current) {
        clearTimeout(confirmTimerRef.current);
        confirmTimerRef.current = null;
      }
      onDelete();
      setConfirmingDelete(false);
    }
  };

  return (
    <div
      className={`group relative w-full text-left rounded-lg transition-colors ${
        active
          ? "bg-primary/10 border border-primary/20"
          : "hover:bg-muted border border-transparent"
      }`}
    >
      <div
        role="button"
        tabIndex={0}
        aria-pressed={active}
        aria-label={`Open chat: ${chat.title}`}
        onClick={() => !confirmingDelete && onClick()}
        onKeyDown={(e) => {
          if (confirmingDelete) return;
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onClick();
          }
        }}
        className="p-3 pr-10 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary focus-visible:ring-offset-2 rounded-lg"
      >
        <div className="flex items-center gap-2 mb-1">
          <span
            className={`w-2 h-2 rounded-full flex-shrink-0 ${statusColors[chat.status] || "bg-gray-300"}`}
          />
          <span className="text-sm font-medium truncate">{chat.title}</span>
          {chat.source && (
            <span className="ml-auto text-[10px] bg-muted text-muted-foreground px-1.5 py-0.5 rounded-full flex-shrink-0 font-mono">
              {chat.source}
            </span>
          )}
        </div>
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span className="flex items-center gap-1">
            <Bot className="w-3 h-3" />
            {chat.assistantName}
            {chat.runParentID && (
              <span className="ml-1 px-1 py-0.5 rounded bg-muted text-[10px] font-mono">
                branched
              </span>
            )}
          </span>
          <span>{new Date(chat.createdAt).toLocaleDateString()}</span>
        </div>
      </div>

      {/* Delete button */}
      <button
        onClick={handleDeleteClick}
        className={`absolute right-2 top-1/2 -translate-y-1/2 p-1.5 rounded-md cursor-pointer transition-all z-10 ${
          confirmingDelete
            ? "bg-red-600 text-white opacity-100"
            : "text-muted-foreground hover:bg-red-100 hover:text-red-600 opacity-0 group-hover:opacity-100"
        }`}
        title={confirmingDelete ? "Click again to confirm" : "Delete chat"}
      >
        {confirmingDelete ? (
          <span className="text-xs font-bold">Sure?</span>
        ) : (
          <Trash2 className="w-3.5 h-3.5" />
        )}
      </button>
    </div>
  );
}

export function ChatInterface({ resetToken = 0 }: { resetToken?: number }) {
  const qc = useQueryClient();

  // Pure local UI state. Nothing here is derived from the server — those
  // fields live in React Query below.
  const [selectedChat, setSelectedChat] = useState<string | null>(null);
  const [selectedAssistant, setSelectedAssistant] = useState("");
  const [newMessage, setNewMessage] = useState("");
  const [processingApproval, setProcessingApproval] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [connectDialogOpen, setConnectDialogOpen] = useState(false);
  const [connectPlugins, setConnectPlugins] = useState<Plugin[]>([]);

  // Scroll management: we need refs (not state) so recalculating doesn't
  // re-render; and the ref values survive across query refetches.
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const messagesContainerRef = useRef<HTMLDivElement>(null);
  const prevStepCountRef = useRef(0);
  const prevStatusRef = useRef<string>("");
  const scrollFrameRef = useRef<number | null>(null);
  const scrollTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [isAtBottom, setIsAtBottom] = useState(true);

  // Track scroll position to detect if user scrolled up
  const handleScroll = () => {
    const container = messagesContainerRef.current;
    if (!container) return;
    const threshold = 100; // px from bottom to consider "at bottom"
    const distanceFromBottom = container.scrollHeight - container.scrollTop - container.clientHeight;
    setIsAtBottom(distanceFromBottom < threshold);
  };

  const scrollToBottom = () => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
    setIsAtBottom(true);
  };

  // Debounced auto-scroll to prevent scroll race conditions
  const debouncedScrollToBottom = useCallback(() => {
    if (scrollTimeoutRef.current) {
      clearTimeout(scrollTimeoutRef.current);
    }
    scrollTimeoutRef.current = setTimeout(() => {
      if (isAtBottom) {
        messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
      }
    }, 100); // 100ms debounce
  }, [isAtBottom]);

  // --- server state via React Query ---

  const assistantsQuery = useQuery({
    queryKey: queryKeys.assistants.list(),
    queryFn: () => assistantsApi.list(),
  });
  // Stable identity: React Query only returns a new array reference when
  // the underlying data actually changes, so memoizing the fallback is the
  // only way to avoid every render triggering dependent hooks to re-run.
  const assistants = useMemo<Assistant[]>(
    () => assistantsQuery.data?.assistants ?? [],
    [assistantsQuery.data],
  );

  const runsQuery = useQuery({
    queryKey: queryKeys.runs.list(),
    queryFn: () => runsApi.list(),
    // EventProvider invalidates this on every run.* event. 60s safety-net
    // refetch in case SSE drops without firing onError.
    refetchInterval: 60_000,
  });
  const runs = useMemo<Run[]>(
    () => runsQuery.data?.runs ?? [],
    [runsQuery.data],
  );

  // Chat detail + approvals: only fetched when a chat is selected.
  // EventProvider invalidates these on step.*/run.*/approval.* events.
  const chatDetailQuery = useQuery({
    queryKey: selectedChat ? queryKeys.runs.detail(selectedChat) : ["runs", "detail", "none"],
    queryFn: () => (selectedChat ? runsApi.get(selectedChat) : Promise.resolve(null)),
    enabled: !!selectedChat,
    // Slightly faster safety net while a run is active; still an order of
    // magnitude slower than the old 2s poll.
    refetchInterval: 10_000,
  });
  const chatData = chatDetailQuery.data ?? null;

  const chatApprovalsQuery = useQuery({
    queryKey: selectedChat
      ? queryKeys.runs.approvals(selectedChat)
      : ["runs", "none", "approvals"],
    queryFn: () =>
      selectedChat
        ? runsApi.getApprovals(selectedChat)
        : Promise.resolve({ approvals: [] }),
    enabled: !!selectedChat,
  });
  const chatApprovals = chatApprovalsQuery.data?.approvals ?? [];

  // --- derived state ---

  const chats: ChatItem[] = useMemo(() => {
    const assistantMap = new Map(assistants.map((a) => [a.id, a.name]));
    return runs.map((run) => ({
      id: run.id,
      title:
        run.goal.length > 60 ? run.goal.slice(0, 60) + "..." : run.goal,
      status: run.status,
      assistantName: assistantMap.get(run.assistant_id) || "Unknown",
      createdAt: run.created_at,
      source: run.source,
      conversationID: run.conversation_id,
      runParentID: run.run_parent_id,
    }));
  }, [runs, assistants]);

  // Group chats by conversationID. Runs with the same conversationID belong
  // to the same multi-turn thread. Ungrouped runs (no conversationID) stand alone.
  const groupedChats: (ChatItem | { __isGroup: true; id: string; conversationID: string; runs: ChatItem[] })[] = useMemo(() => {
    const groups = new Map<string, ChatItem[]>();
    const ungrouped: ChatItem[] = [];

    for (const chat of chats) {
      if (chat.conversationID) {
        const list = groups.get(chat.conversationID) ?? [];
        list.push(chat);
        groups.set(chat.conversationID, list);
      } else {
        ungrouped.push(chat);
      }
    }

    const result: (ChatItem | { __isGroup: true; id: string; conversationID: string; runs: ChatItem[] })[] = [];

    // Sort groups by most recent run
    const sortedGroups = [...groups.entries()].sort((a, b) => {
      const aLatest = Math.max(...a[1].map((r) => new Date(r.createdAt).getTime()));
      const bLatest = Math.max(...b[1].map((r) => new Date(r.createdAt).getTime()));
      return bLatest - aLatest;
    });

    for (const [convID, runs] of sortedGroups) {
      // Sort runs within group by creation time (oldest first)
      runs.sort((a, b) => new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime());
      result.push({
        __isGroup: true,
        id: `group-${convID}`,
        conversationID: convID,
        runs,
      });
    }

    // Add ungrouped runs
    result.push(...ungrouped.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime()));

    return result;
  }, [chats]);

  // Track which conversation groups are expanded
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());

  const toggleGroup = (convID: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(convID)) {
        next.delete(convID);
      } else {
        next.add(convID);
      }
      return next;
    });
  };

  // Seed selectedAssistant once assistants load.
  useEffect(() => {
    if (!selectedAssistant && assistants.length > 0) {
      setSelectedAssistant(assistants[0].id);
    }
  }, [assistants, selectedAssistant]);

  // External reset hook (used by tray "New Chat" action).
  useEffect(() => {
    setSelectedChat(null);
    setNewMessage("");
  }, [resetToken]);

  // Smart scroll: only scroll when new content is added or a terminal
  // transition happens, not on every refetch. refs gate so the effect is
  // idempotent when the query data hasn't meaningfully changed.
  // Only auto-scroll if user is near the bottom (respects user's scroll position).
  useEffect(() => {
    if (!chatData) return;

    const currentStepCount = chatData.steps?.length || 0;
    const currentStatus = chatData.run.status;

    const shouldScroll =
      (currentStepCount > prevStepCountRef.current ||
        (currentStatus !== prevStatusRef.current &&
          (currentStatus === "completed" ||
            currentStatus === "failed" ||
            prevStatusRef.current === "created"))) &&
      isAtBottom; // Only auto-scroll if user hasn't scrolled up

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

  // --- mutations ---

  const createRun = useMutation({
    mutationFn: (goal: string) =>
      runsApi.create({ goal, assistant_id: selectedAssistant }),
    onSuccess: (run) => {
      setNewMessage("");
      setSelectedChat(run.id);
      // Immediately invalidate the runs list; EventProvider will also fire
      // on the run.created event but that round-trips through SSE, so this
      // makes the sidebar update feel instant.
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
      // Roll back the optimistic removal.
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

  // Plan approval + cancel. Both invalidate the run detail so the UI
  // transitions off the plan_review surface as soon as the backend has
  // moved the run forward.
  const approvePlan = useMutation({
    mutationFn: (chatId: string) => runsApi.approvePlan(chatId),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  });

  const editPlan = useMutation({
    mutationFn: ({
      chatId,
      steps,
    }: {
      chatId: string;
      steps: {
        id?: string;
        title: string;
        description?: string;
        expected_tool?: string;
        expected_capability?: string;
        depends_on?: string[];
      }[];
    }) => runsApi.editPlan(chatId, steps),
    onSettled: () => {
      if (selectedChat) {
        qc.invalidateQueries({ queryKey: queryKeys.runs.detail(selectedChat) });
      }
    },
  });

  // "Cancel run" on a plan_review surface transitions the run to cancelled
  // while preserving history in the chat/event timeline.
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

  // --- handlers ---

  const handleSend = () => {
    if (!newMessage.trim() || !selectedAssistant) return;
    createRun.mutate(newMessage);
  };

  const handleApproval = (approvalId: string, approved: boolean, remember: boolean) => {
    resolveApproval.mutate({ id: approvalId, approved, remember });
  };

  const handleDeleteChat = (chatId: string) => {
    deleteRun.mutate(chatId);
  };

  // Get pending approvals for current chat
  const pendingApprovals = chatApprovals.filter((a) => a.status === "pending");

  const getResponseText = () => pickResponseText(chatData?.steps, chatData?.plan);

  // Find the running step (if any) so we can render its streaming buffer.
  // Prefer an llm.chat step since those are what stream tokens; fall back
  // to any running step so other streaming-capable tools we add later
  // light up automatically.
  const runningStep = (() => {
    if (!chatData?.steps) return undefined;
    const running = chatData.steps.filter((s) => s.status === "running");
    if (running.length === 0) return undefined;
    const toolByDef = new Map<string, string | undefined>();
    for (const def of chatData.plan?.steps ?? []) {
      toolByDef.set(def.id, def.expected_tool);
    }
    return (
      running.find(
        (s) => s.step_definition_id && toolByDef.get(s.step_definition_id) === "llm.chat",
      ) ?? running[0]
    );
  })();
  const streamingText = useStepStream(runningStep?.id);

  const loading = runsQuery.isLoading || assistantsQuery.isLoading;
  const sending = createRun.isPending;

  const openConnectDialog = () => {
    setConnectDialogOpen(true);
    pluginsApi
      .list()
      .then((data) => setConnectPlugins(data.plugins))
      .catch(() => setConnectPlugins([]));
  };

  return (
    <div className="flex h-full overflow-hidden relative">
      {deleteError && (
        <div
          role="alert"
          className="absolute top-2 left-1/2 -translate-x-1/2 z-10 bg-destructive/10 border border-destructive text-destructive text-sm rounded-md px-3 py-2 shadow"
        >
          {deleteError}
        </div>
      )}
      {/* Chat List Sidebar */}
      <div className="w-72 border-r flex flex-col bg-muted/20 flex-shrink-0">
        <div className="p-3 border-b flex-shrink-0 space-y-2">
          <Button
            className="w-full"
            onClick={() => {
              setSelectedChat(null);
              setNewMessage("");
            }}
          >
            <Plus className="w-4 h-4 mr-2" />
            New Chat
          </Button>
          <Button
            variant="outline"
            className="w-full"
            onClick={openConnectDialog}
          >
            <Plug className="w-4 h-4 mr-2" />
            Connect
          </Button>
        </div>

        <div className="flex-1 overflow-y-auto p-2 space-y-1 min-h-0">
          {loading && chats.length === 0 ? (
            <div className="p-4 text-sm text-muted-foreground">Loading...</div>
          ) : chats.length === 0 ? (
            <div className="p-4 text-sm text-muted-foreground">
              No chats yet. Start a new conversation!
            </div>
          ) : (
            groupedChats.map((item) => {
              if ("__isGroup" in item) {
                const group = item as { __isGroup: true; id: string; conversationID: string; runs: ChatItem[] };
                const isExpanded = expandedGroups.has(group.conversationID);
                const latestRun = group.runs[group.runs.length - 1];
                const groupTitle = latestRun?.title || "Conversation";
                return (
                  <div key={group.id}>
                    <button
                      type="button"
                      onClick={() => toggleGroup(group.conversationID)}
                      className="w-full text-left px-3 py-2 flex items-center gap-2 rounded-md hover:bg-muted/50 transition-colors"
                      aria-expanded={isExpanded}
                      aria-label={`Conversation: ${groupTitle}`}
                    >
                      <div className={`w-4 h-4 transition-transform ${isExpanded ? "rotate-90" : ""}`}>
                        ▶
                      </div>
                      <span className="text-sm font-medium truncate flex-1">
                        {groupTitle}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {group.runs.length} run{group.runs.length !== 1 ? "s" : ""}
                      </span>
                    </button>
                    {isExpanded && (
                      <div className="ml-4 space-y-1 mt-1">
                        {group.runs.map((run) => (
                          <ChatSidebarItem
                            key={run.id}
                            chat={run}
                            active={selectedChat === run.id}
                            onClick={() => setSelectedChat(run.id)}
                            onDelete={() => handleDeleteChat(run.id)}
                          />
                        ))}
                      </div>
                    )}
                  </div>
                );
              }
              return (
                <ChatSidebarItem
                  key={item.id}
                  chat={item}
                  active={selectedChat === item.id}
                  onClick={() => setSelectedChat(item.id)}
                  onDelete={() => handleDeleteChat(item.id)}
                />
              );
            })
          )}
        </div>
      </div>

      {/* Main Chat Area */}
      <div className="flex-1 flex flex-col min-w-0">
        {!selectedChat ? (
          /* New Chat View */
          <div className="flex-1 flex flex-col items-center justify-center p-8">
            <div className="w-12 h-12 bg-primary rounded-xl flex items-center justify-center mb-4">
              <span className="text-primary-foreground font-bold text-xl">N</span>
            </div>
            <h2 className="text-xl font-semibold mb-2">What can Nomi help you with?</h2>
            <p className="text-sm text-muted-foreground mb-6">
              Select an assistant and start a conversation
            </p>

            <div className="w-full max-w-md space-y-4">
              {assistants.length === 0 ? (
                <div className="text-sm text-muted-foreground text-center">
                  Loading assistants...
                </div>
              ) : (
                <>
                  <select
                    className="w-full h-10 rounded-md border border-input bg-background px-3 py-2 text-sm"
                    value={selectedAssistant}
                    onChange={(e) => setSelectedAssistant(e.target.value)}
                    aria-label="Select an assistant"
                  >
                    {assistants.map((a) => (
                      <option key={a.id} value={a.id}>
                        {a.name} — {a.role}
                      </option>
                    ))}
                  </select>

                  <div className="flex flex-wrap gap-2">
                    {[
                      "Review the README in my workspace and suggest improvements",
                      "List the files in my current workspace and summarize their roles",
                      "Draft a release note for the changes since the last commit",
                    ].map((prompt) => (
                      <button
                        key={prompt}
                        type="button"
                        onClick={() => setNewMessage(prompt)}
                        className="text-xs text-left rounded-full border border-border bg-background hover:bg-muted/40 px-3 py-1.5 transition-colors"
                      >
                        {prompt}
                      </button>
                    ))}
                  </div>

                  <div className="flex gap-2 items-end">
                    <textarea
                      value={newMessage}
                      onChange={(e) => setNewMessage(e.target.value)}
                      placeholder={`Ask ${
                        assistants.find((a) => a.id === selectedAssistant)?.name || "Nomi"
                      } anything... (Shift+Enter for newline)`}
                      onKeyDown={(e) => {
                        if (e.nativeEvent.isComposing || e.keyCode === 229) return;
                        if (e.key === "Enter" && !e.shiftKey) {
                          e.preventDefault();
                          handleSend();
                        }
                      }}
                      rows={2}
                      className="flex-1 min-h-[2.25rem] max-h-40 resize-y rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                    />
                    <Button onClick={handleSend} disabled={sending || !newMessage.trim()}>
                      {sending ? (
                        <Loader2 className="w-4 h-4 animate-spin" />
                      ) : (
                        <Send className="w-4 h-4" />
                      )}
                    </Button>
                  </div>
                </>
              )}
            </div>
          </div>
        ) : (
          /* Active Chat View */
          <>
            {/* Chat Header */}
            <div className="border-b px-4 py-3 flex items-center justify-between flex-shrink-0">
              <div className="min-w-0">
                <h3 className="font-medium truncate">
                  {chatData?.run.goal && chatData.run.goal.length > 80
                    ? chatData.run.goal.slice(0, 80) + "..."
                    : chatData?.run.goal}
                </h3>
                <p className="text-xs text-muted-foreground">
                  {assistants.find((a) => a.id === chatData?.run.assistant_id)?.name || "Nomi"}
                </p>
              </div>
              <div className="flex items-center gap-2 flex-shrink-0">
                {selectedChat &&
                  (chatData?.run.status === "executing" ||
                    chatData?.run.status === "awaiting_approval" ||
                    chatData?.run.status === "paused") && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        if (chatData?.run.status === "paused") {
                          resumeRun.mutate(selectedChat);
                        } else {
                          pauseRun.mutate(selectedChat);
                        }
                      }}
                      disabled={pauseRun.isPending || resumeRun.isPending}
                    >
                      {chatData?.run.status === "paused" ? (
                        <Play className="w-3 h-3 mr-1" />
                      ) : (
                        <Pause className="w-3 h-3 mr-1" />
                      )}
                      {chatData?.run.status === "paused" ? "Resume" : "Pause"}
                    </Button>
                  )}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    if (selectedChat) {
                      qc.invalidateQueries({
                        queryKey: queryKeys.runs.detail(selectedChat),
                      });
                    }
                  }}
                >
                  <RefreshCw className="w-3 h-3 mr-1" />
                  Refresh
                </Button>
                <Badge
                  variant={
                    chatData?.run.status === "completed"
                      ? "default"
                      : chatData?.run.status === "failed"
                        ? "destructive"
                        : "secondary"
                  }
                >
                  {chatData?.run.status}
                </Badge>
                {chatData?.run.run_parent_id && (
                  <Badge variant="outline" className="font-mono text-[10px]">
                    branched from {chatData.run.run_parent_id.slice(0, 8)}
                  </Badge>
                )}
              </div>
            </div>

            {/* Messages - scrollable area */}
            <div
              ref={messagesContainerRef}
              className="flex-1 overflow-y-auto p-4 space-y-4 min-h-0 relative"
              role="log"
              aria-live="polite"
              onScroll={handleScroll}
            >
              {/* User Message */}
              <div className="flex justify-end">
                <div className="bg-primary text-primary-foreground rounded-2xl rounded-tr-sm px-4 py-3 max-w-[80%]">
                  <p className="text-sm">{chatData?.run.goal}</p>
                </div>
              </div>

              {/* Plan Review — blocks until the user approves or cancels */}
              {chatData?.run.status === "plan_review" && chatData.plan && (
                <div className="flex justify-start">
                  <div className="max-w-[80%] w-full space-y-2">
                    <div className="flex items-center gap-2">
                      <div className="w-6 h-6 bg-primary/10 rounded-full flex items-center justify-center">
                        <Bot className="w-3.5 h-3.5 text-primary" />
                      </div>
                      <span className="text-xs font-medium text-muted-foreground">
                        {assistants.find((a) => a.id === chatData.run.assistant_id)?.name || "Nomi"}
                      </span>
                    </div>
                    <PlanReviewCard
                      plan={chatData.plan}
                      onApprove={() => selectedChat && approvePlan.mutate(selectedChat)}
                      onEdit={(steps) =>
                        selectedChat && editPlan.mutate({ chatId: selectedChat, steps })
                      }
                      onFork={(stepID) =>
                        selectedChat && forkRun.mutate({ chatId: selectedChat, stepId: stepID })
                      }
                      onCancel={() => selectedChat && cancelPlan.mutate(selectedChat)}
                      approving={approvePlan.isPending}
                      editing={editPlan.isPending}
                      cancelling={cancelPlan.isPending}
                    />
                  </div>
                </div>
              )}

              {/* Thinking / Status Block. Hidden during plan_review so the
                  PlanReviewCard is the only actionable surface. */}
              {chatData && chatData.run.status !== "created" && chatData.run.status !== "plan_review" && !getResponseText() && (
                <div className="flex justify-start">
                  <div className="max-w-[80%] space-y-2">
                    <div className="flex items-center gap-2">
                      <div className="w-6 h-6 bg-primary/10 rounded-full flex items-center justify-center">
                        <Bot className="w-3.5 h-3.5 text-primary" />
                      </div>
                      <span className="text-xs font-medium text-muted-foreground">
                        {assistants.find((a) => a.id === chatData.run.assistant_id)?.name || "Nomi"}
                      </span>
                    </div>
                    <ThinkingBlock
                      status={chatData.run.status}
                      steps={chatData.steps}
                      plan={chatData.plan}
                      agentName={assistants.find((a) => a.id === chatData.run.assistant_id)?.name}
                    />
                  </div>
                </div>
              )}

              {/* Live streaming buffer for the currently-running step.
                  Hidden when the step has produced no tokens yet (so we
                  don't render an empty bubble) and once the persisted
                  output supersedes the live text via getResponseText(). */}
              {chatData && streamingText && !getResponseText() && (
                <div className="flex justify-start">
                  <div className="max-w-[80%] space-y-2">
                    <div className="flex items-center gap-2">
                      <div className="w-6 h-6 bg-primary/10 rounded-full flex items-center justify-center">
                        <Bot className="w-3.5 h-3.5 text-primary" />
                      </div>
                      <span className="text-xs font-medium text-muted-foreground">
                        {assistants.find((a) => a.id === chatData.run.assistant_id)?.name || "Nomi"}
                      </span>
                    </div>
                    <div
                      className="bg-muted rounded-2xl rounded-tl-sm px-4 py-3"
                      aria-live="polite"
                      aria-atomic="false"
                    >
                      <p className="text-sm whitespace-pre-wrap">
                        {streamingText}
                        <span className="ml-0.5 inline-block w-1.5 h-3 bg-foreground/60 align-baseline animate-pulse" aria-hidden="true" />
                      </p>
                    </div>
                  </div>
                </div>
              )}

              {/* Approval Requests */}
              {pendingApprovals.map((approval) => (
                <div key={approval.id} className="flex justify-start">
                  <div className="max-w-[80%]">
                    <ApprovalCard
                      approval={approval}
                      onResolve={(approved, remember) => handleApproval(approval.id, approved, remember)}
                      processing={processingApproval === approval.id}
                      agentName={assistants.find((a) => a.id === chatData?.run.assistant_id)?.name}
                    />
                  </div>
                </div>
              ))}

              {/* Assistant Response */}
              {getResponseText() && (
                <div className="flex justify-start">
                  <div className="max-w-[80%] space-y-2">
                    <div className="flex items-center gap-2">
                      <div className="w-6 h-6 bg-primary/10 rounded-full flex items-center justify-center">
                        <Bot className="w-3.5 h-3.5 text-primary" />
                      </div>
                      <span className="text-xs font-medium text-muted-foreground">
                        {assistants.find((a) => a.id === chatData?.run.assistant_id)?.name || "Nomi"}
                      </span>
                    </div>
                    <div className="bg-muted rounded-2xl rounded-tl-sm px-4 py-3">
                      <p className="text-sm whitespace-pre-wrap">{getResponseText()}</p>
                    </div>
                  </div>
                </div>
              )}

              {/* Error State */}
              {chatData?.run.status === "failed" && chatData.steps.some((s) => s.error) && (
                <div className="flex justify-start">
                  <div className="max-w-[80%] bg-red-50 border border-red-200 rounded-lg px-4 py-3">
                    <p className="text-sm text-red-700">
                      {chatData.steps.find((s) => s.error)?.error}
                    </p>
                  </div>
                </div>
              )}

              <div ref={messagesEndRef} />
              
              {/* Scroll to bottom button - appears when user scrolls up */}
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

            {/* Input - fixed at bottom. Disabled while the plan is under
                review so the user can't accidentally discard the plan by
                typing Enter — they must approve or cancel first. */}
            <div className="border-t p-4 flex-shrink-0">
              <div className="flex gap-2 max-w-4xl mx-auto items-end">
                <textarea
                  value={newMessage}
                  onChange={(e) => setNewMessage(e.target.value)}
                  placeholder={
                    chatData?.run.status === "plan_review"
                      ? "Waiting on your review"
                      : "Send a message... (Shift+Enter for newline)"
                  }
                  onKeyDown={(e) => {
                    // Skip when an IME composition is in progress so users
                    // typing CJK candidates don't send the partial input.
                    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
                    if (e.key === "Enter" && !e.shiftKey) {
                      e.preventDefault();
                      handleSend();
                    }
                  }}
                  disabled={chatData?.run.status === "plan_review"}
                  rows={1}
                  className="flex-1 min-h-[2.25rem] max-h-40 resize-y rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
                />
                <Button
                  onClick={handleSend}
                  disabled={
                    sending ||
                    !newMessage.trim() ||
                    !selectedAssistant ||
                    chatData?.run.status === "plan_review"
                  }
                >
                  {sending ? (
                    <Loader2 className="w-4 h-4 animate-spin" />
                  ) : (
                    <Send className="w-4 h-4" />
                  )}
                </Button>
              </div>
            </div>
          </>
        )}
      </div>

      <Dialog open={connectDialogOpen} onOpenChange={setConnectDialogOpen}>
        <DialogContent className="max-w-2xl max-h-[90vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Connect services</DialogTitle>
          </DialogHeader>
          <OutcomeConnectorPicker
            plugins={connectPlugins}
            onSkip={() => setConnectDialogOpen(false)}
            onDone={() => setConnectDialogOpen(false)}
            mode="modal"
          />
        </DialogContent>
      </Dialog>
    </div>
  );
}
