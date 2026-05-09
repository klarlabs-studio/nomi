import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";

/**
 * Bottom-bar composer for an active chat. Disabled while the run is in
 * `plan_review` so the user can't accidentally discard the plan by
 * pressing Enter — they must approve or cancel first.
 */
export function ChatComposer({
  newMessage,
  onNewMessageChange,
  onSend,
  sending,
  disabled,
  selectedAssistant,
  status,
}: {
  newMessage: string;
  onNewMessageChange: (msg: string) => void;
  onSend: () => void;
  sending: boolean;
  disabled: boolean;
  selectedAssistant: string;
  status: string;
}) {
  return (
    <div className="border-t p-4 flex-shrink-0">
      <div className="flex gap-2 max-w-4xl mx-auto items-end">
        <textarea
          value={newMessage}
          onChange={(e) => onNewMessageChange(e.target.value)}
          placeholder={
            status === "plan_review"
              ? "Waiting on your review"
              : "Send a message... (Shift+Enter for newline)"
          }
          onKeyDown={(e) => {
            // Skip when an IME composition is in progress so users typing
            // CJK candidates don't send the partial input.
            if (e.nativeEvent.isComposing || e.keyCode === 229) return;
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              onSend();
            }
          }}
          disabled={disabled}
          rows={1}
          className="flex-1 min-h-[2.25rem] max-h-40 resize-y rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
        />
        <Button
          onClick={onSend}
          disabled={
            sending ||
            !newMessage.trim() ||
            !selectedAssistant ||
            disabled
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
  );
}
