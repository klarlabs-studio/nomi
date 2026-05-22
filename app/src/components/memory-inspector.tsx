import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { memoryApi } from "@/lib/api";
import { errorMessage } from "@/lib/utils";
import { queryKeys } from "@/lib/query-keys";
import type { Memory } from "@/types/api";

export function MemoryInspector() {
  const qc = useQueryClient();
  const [searchQuery, setSearchQuery] = useState("");
  const [newContent, setNewContent] = useState("");
  const [newScope, setNewScope] = useState("workspace");

  const params = searchQuery ? { q: searchQuery } : undefined;

  // Event-driven: EventProvider invalidates queryKeys.memory.all on every
  // memory.* event. 60s safety refetch catches SSE drops.
  const { data, isLoading, error: queryError } = useQuery({
    queryKey: queryKeys.memory.list(params),
    queryFn: () => memoryApi.list(params),
    refetchInterval: 60_000,
  });
  const memories = data?.memories ?? [];

  const createMutation = useMutation({
    mutationFn: (p: { content: string; scope: string }) =>
      memoryApi.create({ content: p.content, scope: p.scope }),
    onSuccess: () => {
      setNewContent("");
      qc.invalidateQueries({ queryKey: queryKeys.memory.all });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => memoryApi.delete(id),
    onSettled: () => qc.invalidateQueries({ queryKey: queryKeys.memory.all }),
  });

  const handleCreate = () => {
    if (!newContent.trim()) return;
    createMutation.mutate({ content: newContent, scope: newScope });
  };

  const handleDelete = (id: string) => {
    deleteMutation.mutate(id);
  };

  // Error surface aggregates all three possible sources so the banner
  // below behaves like the original component.
  const error = queryError
    ? errorMessage(queryError)
    : createMutation.error
      ? errorMessage(createMutation.error)
      : deleteMutation.error
        ? errorMessage(deleteMutation.error)
        : null;
  const loading = isLoading;
  const creating = createMutation.isPending;

  const profileMemories = memories.filter((m) => m.scope === "profile");
  const workspaceMemories = memories.filter((m) => m.scope === "workspace");
  const preferenceMemories = memories.filter((m) => m.scope === "preferences");

  if (loading && memories.length === 0) {
    return (
      <div className="p-4 flex items-center justify-center h-full">
        <div className="text-muted-foreground">Loading memories...</div>
      </div>
    );
  }

  return (
    <div className="p-4 space-y-4 h-full flex flex-col">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Memory Inspector</h2>
      </div>

      {error && (
        <div className="bg-destructive/10 text-destructive p-3 rounded-md text-sm">
          <p className="font-medium">Error</p>
          <p>{error}</p>
          <Button
            variant="outline"
            size="sm"
            onClick={() => qc.invalidateQueries({ queryKey: queryKeys.memory.all })}
            className="mt-2"
          >
            Retry
          </Button>
        </div>
      )}

      {/* Create Memory */}
      <div className="space-y-2 border rounded-lg p-3">
        <h3 className="text-sm font-medium">Add Memory</h3>
        <div className="flex gap-2">
          <select
            className="flex h-9 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
            value={newScope}
            onChange={(e) => setNewScope(e.target.value)}
          >
            <option value="workspace">Workspace</option>
            <option value="profile">Profile</option>
          </select>
          <Input
            placeholder="What should I remember?"
            value={newContent}
            onChange={(e) => setNewContent(e.target.value)}
            className="flex-1"
          />
          <Button onClick={handleCreate} disabled={creating} size="sm">
            {creating ? "Saving..." : "Save"}
          </Button>
        </div>
      </div>

      {/* Search */}
      <Input
        placeholder="Search memories..."
        value={searchQuery}
        onChange={(e) => setSearchQuery(e.target.value)}
      />

      {/* Memory Tabs */}
      {memories.length === 0 ? (
        <div className="text-muted-foreground text-center py-8">
          <p>No memories stored yet.</p>
          <p className="text-sm mt-1">Add a memory above or run an assistant to generate memories.</p>
        </div>
      ) : (
        <Tabs defaultValue="workspace" className="flex-1 flex flex-col min-h-0">
          <TabsList>
            <TabsTrigger value="workspace">
              Workspace ({workspaceMemories.length})
            </TabsTrigger>
            <TabsTrigger value="profile">
              Profile ({profileMemories.length})
            </TabsTrigger>
            <TabsTrigger value="preferences">
              Preferences ({preferenceMemories.length})
            </TabsTrigger>
          </TabsList>
          <TabsContent value="workspace" className="flex-1 overflow-auto space-y-2 mt-2">
            {workspaceMemories.length === 0 ? (
              <div className="text-muted-foreground text-center py-4">No workspace memories.</div>
            ) : (
              workspaceMemories.map((memory) => (
                <MemoryCard key={memory.id} memory={memory} onDelete={handleDelete} />
              ))
            )}
          </TabsContent>
          <TabsContent value="profile" className="flex-1 overflow-auto space-y-2 mt-2">
            {profileMemories.length === 0 ? (
              <div className="text-muted-foreground text-center py-4">No profile memories.</div>
            ) : (
              profileMemories.map((memory) => (
                <MemoryCard key={memory.id} memory={memory} onDelete={handleDelete} />
              ))
            )}
          </TabsContent>
          <TabsContent value="preferences" className="flex-1 overflow-auto space-y-3 mt-2">
            <div className="text-xs text-muted-foreground bg-muted rounded-md p-2">
              These entries shape future planning behavior. Auto-learned entries are extracted
              after successful runs; you stay in control — delete any that don&apos;t reflect what
              you actually want. The planner reads from this scope and annotates plans with the
              preferences that influenced them.
            </div>
            {preferenceMemories.length === 0 ? (
              <div className="text-muted-foreground text-center py-4">No learned preferences yet.</div>
            ) : (
              <>
                {(() => {
                  const inferred = preferenceMemories.filter((m) =>
                    m.content.startsWith("Inferred: "),
                  );
                  const manual = preferenceMemories.filter(
                    (m) => !m.content.startsWith("Inferred: "),
                  );
                  return (
                    <>
                      {inferred.length > 0 && (
                        <div className="space-y-2">
                          <div className="text-xs font-medium text-muted-foreground">
                            Auto-learned ({inferred.length})
                          </div>
                          {inferred.map((memory) => (
                            <MemoryCard
                              key={memory.id}
                              memory={memory}
                              onDelete={handleDelete}
                              autoLearned
                            />
                          ))}
                        </div>
                      )}
                      {manual.length > 0 && (
                        <div className="space-y-2">
                          <div className="text-xs font-medium text-muted-foreground">
                            Manual ({manual.length})
                          </div>
                          {manual.map((memory) => (
                            <MemoryCard key={memory.id} memory={memory} onDelete={handleDelete} />
                          ))}
                        </div>
                      )}
                    </>
                  );
                })()}
              </>
            )}
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}

function MemoryCard({
  memory,
  onDelete,
  autoLearned,
}: {
  memory: Memory;
  onDelete: (id: string) => void;
  autoLearned?: boolean;
}) {
  const displayContent = autoLearned
    ? memory.content.replace(/^Inferred: /, "")
    : memory.content;
  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-1">
            <Badge variant="outline">{memory.scope}</Badge>
            {autoLearned && (
              <Badge
                variant="secondary"
                className="text-xs"
                title="Extracted by Nomi from a successful run"
              >
                auto-learned
              </Badge>
            )}
          </div>
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">
              {new Date(memory.created_at).toLocaleString()}
            </span>
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-2 text-destructive hover:text-destructive"
              onClick={() => onDelete(memory.id)}
            >
              Delete
            </Button>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <div className="text-sm whitespace-pre-wrap">{displayContent}</div>
        <div className="mt-2 text-xs text-muted-foreground space-y-1">
          {memory.assistant_id && <div>Assistant: {memory.assistant_id.slice(0, 8)}...</div>}
          {memory.run_id && <div>From run: {memory.run_id.slice(0, 8)}...</div>}
        </div>
      </CardContent>
    </Card>
  );
}
