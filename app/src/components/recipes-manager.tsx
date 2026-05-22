import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { recipesApi, type RecipeCatalogEntry } from "@/lib/api";

// Recipes tab — browse the built-in catalog + imported/exported recipes
// and install a recipe as a fresh assistant. Minimal v1: no inline
// diff/preview UI — install button POSTs straight after a confirm()
// prompt. The /recipes/:id/preview endpoint exists for a future
// expansion that surfaces the assistant spec inline before commit.
export function RecipesManager() {
  const [items, setItems] = useState<RecipeCatalogEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [installing, setInstalling] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [installedRecipeID, setInstalledRecipeID] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    recipesApi
      .list()
      .then((data) => {
        if (cancelled) return;
        setItems(data.recipes);
        setError(null);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const install = async (entry: RecipeCatalogEntry) => {
    if (!confirm(`Install "${entry.name}" as a new assistant?`)) return;
    setInstalling(entry.id);
    try {
      const result = await recipesApi.install(entry.id, entry.sha256);
      setInstalledRecipeID(result.recipe_id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setInstalling(null);
    }
  };

  if (loading) return <div className="p-4 text-sm text-muted-foreground">Loading recipes…</div>;

  return (
    <div className="space-y-4 p-4">
      <div>
        <h2 className="text-lg font-semibold">Recipes</h2>
        <p className="text-sm text-muted-foreground">
          Versioned assistant bundles. Install one to spin up a new assistant with its capabilities,
          permission policy, and execution backend already configured.
        </p>
      </div>

      {error && (
        <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm">
          {error}
        </div>
      )}

      {installedRecipeID && (
        <div className="rounded-md border border-green-500/40 bg-green-500/10 p-3 text-sm">
          Installed recipe <code className="font-mono">{installedRecipeID}</code>. Open the
          Assistants tab to see your new assistant.
        </div>
      )}

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        {items.map((entry) => (
          <Card key={`${entry.id}-${entry.source}`}>
            <CardHeader>
              <div className="flex items-start justify-between gap-2">
                <CardTitle className="text-base">{entry.name}</CardTitle>
                <Badge variant={entry.source === "builtin" ? "default" : "outline"}>
                  {entry.source}
                </Badge>
              </div>
              <div className="text-xs text-muted-foreground">
                v{entry.version}
                {entry.author ? ` · ${entry.author}` : ""}
              </div>
            </CardHeader>
            <CardContent className="space-y-3">
              {entry.description && (
                <p className="text-sm text-muted-foreground whitespace-pre-line">
                  {entry.description}
                </p>
              )}
              {entry.tags && entry.tags.length > 0 && (
                <div className="flex flex-wrap gap-1">
                  {entry.tags.map((t) => (
                    <Badge key={t} variant="secondary" className="text-xs">
                      {t}
                    </Badge>
                  ))}
                </div>
              )}
              <div className="flex items-center justify-between gap-2 pt-1">
                {entry.sha256 && (
                  <code className="text-xs text-muted-foreground" title={entry.sha256}>
                    sha256:{entry.sha256.slice(0, 12)}…
                  </code>
                )}
                <Button
                  size="sm"
                  onClick={() => install(entry)}
                  disabled={installing === entry.id}
                >
                  {installing === entry.id ? "Installing…" : "Install"}
                </Button>
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
