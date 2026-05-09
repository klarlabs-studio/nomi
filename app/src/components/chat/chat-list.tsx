import { useEffect, useRef, useState } from "react";
import { Bot, Plug, Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { ChatItem, GroupedChatItem } from "./types";

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

export function ChatList({
  chats,
  groupedChats,
  selectedChat,
  expandedGroups,
  loading,
  onNewChat,
  onOpenConnect,
  onSelect,
  onToggleGroup,
  onDelete,
}: {
  chats: ChatItem[];
  groupedChats: GroupedChatItem[];
  selectedChat: string | null;
  expandedGroups: Set<string>;
  loading: boolean;
  onNewChat: () => void;
  onOpenConnect: () => void;
  onSelect: (id: string) => void;
  onToggleGroup: (conversationID: string) => void;
  onDelete: (id: string) => void;
}) {
  // Client-side search keeps the round-trip out of the keystroke
  // path; once a user has thousands of runs, swap to /runs?search=
  // (already wired server-side). The match is case-insensitive
  // substring against ChatItem.title.
  const [searchQuery, setSearchQuery] = useState("");
  const q = searchQuery.trim().toLowerCase();
  const filteredGroups: GroupedChatItem[] = q
    ? groupedChats
        .map((item) => {
          if ("__isGroup" in item) {
            const matchedRuns = item.runs.filter((r) =>
              r.title.toLowerCase().includes(q),
            );
            if (matchedRuns.length === 0) return null;
            return { ...item, runs: matchedRuns };
          }
          return item.title.toLowerCase().includes(q) ? item : null;
        })
        .filter((x): x is GroupedChatItem => x !== null)
    : groupedChats;

  return (
    <div className="w-72 border-r flex flex-col bg-muted/20 flex-shrink-0">
      <div className="p-3 border-b flex-shrink-0 space-y-2">
        <Button className="w-full" onClick={onNewChat}>
          <Plus className="w-4 h-4 mr-2" />
          New Chat
        </Button>
        <Button variant="outline" className="w-full" onClick={onOpenConnect}>
          <Plug className="w-4 h-4 mr-2" />
          Connect
        </Button>
        <Input
          type="search"
          placeholder="Search runs..."
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          className="h-8 text-sm"
          aria-label="Search runs by title"
        />
      </div>

      <div className="flex-1 overflow-y-auto p-2 space-y-1 min-h-0">
        {loading && chats.length === 0 ? (
          <div className="p-4 text-sm text-muted-foreground">Loading...</div>
        ) : chats.length === 0 ? (
          <div className="p-4 text-sm text-muted-foreground">
            No chats yet. Start a new conversation!
          </div>
        ) : filteredGroups.length === 0 ? (
          <div className="p-4 text-sm text-muted-foreground">
            No runs match &quot;{searchQuery}&quot;.
          </div>
        ) : (
          filteredGroups.map((item) => {
            if ("__isGroup" in item) {
              const isExpanded = expandedGroups.has(item.conversationID);
              const latestRun = item.runs[item.runs.length - 1];
              const groupTitle = latestRun?.title || "Conversation";
              return (
                <div key={item.id}>
                  <button
                    type="button"
                    onClick={() => onToggleGroup(item.conversationID)}
                    className="w-full text-left px-3 py-2 flex items-center gap-2 rounded-md hover:bg-muted/50 transition-colors"
                    aria-expanded={isExpanded}
                    aria-label={`Conversation: ${groupTitle}`}
                  >
                    <div
                      className={`w-4 h-4 transition-transform ${
                        isExpanded ? "rotate-90" : ""
                      }`}
                    >
                      ▶
                    </div>
                    <span className="text-sm font-medium truncate flex-1">
                      {groupTitle}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {item.runs.length} run{item.runs.length !== 1 ? "s" : ""}
                    </span>
                  </button>
                  {isExpanded && (
                    <div className="ml-4 space-y-1 mt-1">
                      {item.runs.map((run) => (
                        <ChatSidebarItem
                          key={run.id}
                          chat={run}
                          active={selectedChat === run.id}
                          onClick={() => onSelect(run.id)}
                          onDelete={() => onDelete(run.id)}
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
                onClick={() => onSelect(item.id)}
                onDelete={() => onDelete(item.id)}
              />
            );
          })
        )}
      </div>
    </div>
  );
}
