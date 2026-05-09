import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { assistantsApi, pluginsApi, runsApi } from "@/lib/api";
import { OutcomeConnectorPicker } from "@/components/onboarding/outcome-connectors";
import { queryKeys } from "@/lib/query-keys";
import { useStepStream } from "@/lib/streaming";
import type { Assistant, Plugin, Run } from "@/types/api";
import { ChatList } from "@/components/chat/chat-list";
import { ChatDetail } from "@/components/chat/chat-detail";
import { NewChatView } from "@/components/chat/new-chat-view";
import type {
  ChatItem,
  ConversationGroup,
  GroupedChatItem,
} from "@/components/chat/types";
import { useChatActions } from "@/components/chat/use-chat-actions";

export function ChatInterface({ resetToken = 0 }: { resetToken?: number }) {
  // Pure local UI state. Nothing here is derived from the server — those
  // fields live in React Query below.
  const [selectedChat, setSelectedChat] = useState<string | null>(null);
  const [selectedAssistant, setSelectedAssistant] = useState("");
  const [newMessage, setNewMessage] = useState("");
  const [connectDialogOpen, setConnectDialogOpen] = useState(false);
  const [connectPlugins, setConnectPlugins] = useState<Plugin[]>([]);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());

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
  const pendingApprovals = chatApprovals.filter((a) => a.status === "pending");

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

  // Group chats by conversationID. Runs sharing a conversationID belong
  // to the same multi-turn thread; ungrouped runs stand alone.
  const groupedChats: GroupedChatItem[] = useMemo(() => {
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

    const result: GroupedChatItem[] = [];
    const sortedGroups = [...groups.entries()].sort((a, b) => {
      const aLatest = Math.max(...a[1].map((r) => new Date(r.createdAt).getTime()));
      const bLatest = Math.max(...b[1].map((r) => new Date(r.createdAt).getTime()));
      return bLatest - aLatest;
    });
    for (const [convID, runs] of sortedGroups) {
      runs.sort(
        (a, b) => new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime(),
      );
      const group: ConversationGroup = {
        __isGroup: true,
        id: `group-${convID}`,
        conversationID: convID,
        runs,
      };
      result.push(group);
    }
    result.push(
      ...ungrouped.sort(
        (a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime(),
      ),
    );
    return result;
  }, [chats]);

  const toggleGroup = (convID: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(convID)) next.delete(convID);
      else next.add(convID);
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

  // --- mutations + streaming ---

  const actions = useChatActions({
    selectedChat,
    selectedAssistant,
    setSelectedChat,
    setNewMessage,
  });

  // Find the running step for streaming. Prefer an llm.chat step since
  // those stream tokens; fall back to any running step so streaming-
  // capable tools added later light up automatically.
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

  // --- handlers ---

  const handleSend = () => {
    if (!newMessage.trim() || !selectedAssistant) return;
    actions.createRun.mutate(newMessage);
  };

  const handleNewChat = () => {
    setSelectedChat(null);
    setNewMessage("");
  };

  const openConnectDialog = () => {
    setConnectDialogOpen(true);
    pluginsApi
      .list()
      .then((data) => setConnectPlugins(data.plugins))
      .catch(() => setConnectPlugins([]));
  };

  const loading = runsQuery.isLoading || assistantsQuery.isLoading;
  const sending = actions.createRun.isPending;

  return (
    <div className="flex h-full overflow-hidden relative">
      {actions.deleteError && (
        <div
          role="alert"
          className="absolute top-2 left-1/2 -translate-x-1/2 z-10 bg-destructive/10 border border-destructive text-destructive text-sm rounded-md px-3 py-2 shadow"
        >
          {actions.deleteError}
        </div>
      )}

      <ChatList
        chats={chats}
        groupedChats={groupedChats}
        selectedChat={selectedChat}
        expandedGroups={expandedGroups}
        loading={loading}
        onNewChat={handleNewChat}
        onOpenConnect={openConnectDialog}
        onSelect={setSelectedChat}
        onToggleGroup={toggleGroup}
        onDelete={(id) => actions.deleteRun.mutate(id)}
      />

      <div className="flex-1 flex flex-col min-w-0">
        {!selectedChat || !chatData ? (
          <NewChatView
            assistants={assistants}
            selectedAssistant={selectedAssistant}
            onSelectAssistant={setSelectedAssistant}
            newMessage={newMessage}
            onNewMessageChange={setNewMessage}
            onSend={handleSend}
            sending={sending}
          />
        ) : (
          <ChatDetail
            chatData={chatData}
            assistants={assistants}
            pendingApprovals={pendingApprovals}
            streamingText={streamingText}
            processingApproval={actions.processingApproval}
            newMessage={newMessage}
            onNewMessageChange={setNewMessage}
            onSend={handleSend}
            sending={sending}
            selectedAssistant={selectedAssistant}
            onResolveApproval={(id, approved, remember) =>
              actions.resolveApproval.mutate({ id, approved, remember })
            }
            onApprovePlan={() =>
              selectedChat && actions.approvePlan.mutate(selectedChat)
            }
            onEditPlan={(steps) =>
              selectedChat &&
              actions.editPlan.mutate({ chatId: selectedChat, steps })
            }
            onForkRun={(stepID) =>
              selectedChat &&
              actions.forkRun.mutate({ chatId: selectedChat, stepId: stepID })
            }
            onCancelPlan={() =>
              selectedChat && actions.cancelPlan.mutate(selectedChat)
            }
            approvePlanPending={actions.approvePlan.isPending}
            editPlanPending={actions.editPlan.isPending}
            cancelPlanPending={actions.cancelPlan.isPending}
            onPause={() => selectedChat && actions.pauseRun.mutate(selectedChat)}
            onResume={() => selectedChat && actions.resumeRun.mutate(selectedChat)}
            pausePending={actions.pauseRun.isPending}
            resumePending={actions.resumeRun.isPending}
            onRefresh={actions.refreshChat}
          />
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

