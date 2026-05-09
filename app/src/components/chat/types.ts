export interface ChatItem {
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
  // sidebar (see groupByConversation in chat-list.tsx).
  conversationID?: string;
  runParentID?: string;
}

export type ConversationGroup = {
  __isGroup: true;
  id: string;
  conversationID: string;
  runs: ChatItem[];
};

export type GroupedChatItem = ChatItem | ConversationGroup;
