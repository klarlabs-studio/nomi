import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Select, SelectItem } from "@/components/ui/select";
import type { Assistant } from "@/types/api";

const STARTER_PROMPTS = [
  "Review the README in my workspace and suggest improvements",
  "List the files in my current workspace and summarize their roles",
  "Draft a release note for the changes since the last commit",
];

export function NewChatView({
  assistants,
  selectedAssistant,
  onSelectAssistant,
  newMessage,
  onNewMessageChange,
  onSend,
  sending,
}: {
  assistants: Assistant[];
  selectedAssistant: string;
  onSelectAssistant: (id: string) => void;
  newMessage: string;
  onNewMessageChange: (msg: string) => void;
  onSend: () => void;
  sending: boolean;
}) {
  const activeName =
    assistants.find((a) => a.id === selectedAssistant)?.name || "Nomi";

  return (
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
            <Select
              value={selectedAssistant}
              onValueChange={onSelectAssistant}
              placeholder="Select an assistant"
              disabled={assistants.length === 0}
              className="w-full max-w-md"
            >
              {assistants.map((a) => (
                <SelectItem key={a.id} value={a.id}>
                  {a.name} — {a.role}
                </SelectItem>
              ))}
            </Select>

            <div className="flex flex-wrap gap-2">
              {STARTER_PROMPTS.map((prompt) => (
                <button
                  key={prompt}
                  type="button"
                  onClick={() => onNewMessageChange(prompt)}
                  className="text-xs text-left rounded-full border border-border bg-background hover:bg-muted/40 px-3 py-1.5 transition-colors"
                >
                  {prompt}
                </button>
              ))}
            </div>

            <div className="flex gap-2 items-end">
              <textarea
                value={newMessage}
                onChange={(e) => onNewMessageChange(e.target.value)}
                placeholder={`Ask ${activeName} anything... (Shift+Enter for newline)`}
                onKeyDown={(e) => {
                  if (e.nativeEvent.isComposing || e.keyCode === 229) return;
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    onSend();
                  }
                }}
                rows={2}
                className="flex-1 min-h-[2.25rem] max-h-40 resize-y rounded-md border border-input bg-background px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
              />
              <Button onClick={onSend} disabled={sending || !newMessage.trim()}>
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
  );
}
