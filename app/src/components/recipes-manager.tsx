import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import {
  recipesApi,
  skillsApi,
  type RecipeCatalogEntry,
  type SkillSuggestion,
  type SynthesizedRecipe,
} from "@/lib/api";

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

  // Skill induction state. Suggestions are fetched lazily — the panel
  // collapses by default so cold loads don't always scan the run history.
  const [suggestions, setSuggestions] = useState<SkillSuggestion[]>([]);
  const [suggestionsLoading, setSuggestionsLoading] = useState(false);
  const [suggestionsLoaded, setSuggestionsLoaded] = useState(false);
  const [promoteFor, setPromoteFor] = useState<string | null>(null);
  const [promoteName, setPromoteName] = useState("");
  const [synthesizing, setSynthesizing] = useState<string | null>(null);
  const [synthesisFor, setSynthesisFor] = useState<string | null>(null);
  const [synthesis, setSynthesis] = useState<SynthesizedRecipe | null>(null);

  const refreshSuggestions = async () => {
    setSuggestionsLoading(true);
    setError(null);
    try {
      const data = await skillsApi.listSuggestions();
      setSuggestions(data.suggestions);
      setSuggestionsLoaded(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSuggestionsLoading(false);
    }
  };

  const synthesize = async (s: SkillSuggestion) => {
    setSynthesizing(s.id);
    setError(null);
    try {
      const result = await skillsApi.synthesize(s.id);
      setSynthesis(result.recipe);
      setSynthesisFor(s.id);
      setPromoteFor(s.id);
      setPromoteName(result.recipe.suggested_name);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSynthesizing(null);
    }
  };

  const promote = async (s: SkillSuggestion) => {
    if (!promoteName.trim()) {
      setError("Choose a name for the new skill before promoting.");
      return;
    }
    const recipeID = promoteName
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "") || `skill-${s.id}`;
    try {
      const useSynthesis = synthesisFor === s.id && synthesis;
      const result = await skillsApi.promote({
        suggestion_id: s.id,
        recipe_id: recipeID,
        name: promoteName.trim(),
        source_assistant_id: s.suggested_assistant_id,
        synthesized_role: useSynthesis ? synthesis?.suggested_role : undefined,
        synthesized_system_prompt: useSynthesis ? synthesis?.system_prompt : undefined,
        synthesized_capabilities: useSynthesis ? synthesis?.capabilities : undefined,
      });
      setInstalledRecipeID(result.recipe.id);
      setPromoteFor(null);
      setPromoteName("");
      // Refresh both panels so the new recipe shows up immediately.
      const data = await recipesApi.list();
      setItems(data.recipes);
      await refreshSuggestions();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

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

      {/* Suggested skills — derived from past successful runs. Collapsed
          by default; clicking the toggle triggers a fresh induction
          pass on the run history. */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-2">
            <CardTitle className="text-base">Suggested skills</CardTitle>
            <Button size="sm" variant="outline" onClick={() => void refreshSuggestions()}>
              {suggestionsLoading ? "Scanning…" : suggestionsLoaded ? "Refresh" : "Scan history"}
            </Button>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-xs text-muted-foreground">
            Mines your past successful runs and surfaces clusters of similar work as candidate
            recipes. Promote one to turn the cluster into a reusable assistant.
          </p>
          {suggestionsLoaded && suggestions.length === 0 && (
            <p className="text-sm text-muted-foreground">
              No clusters above the threshold yet — try again after you&apos;ve run a few similar
              tasks.
            </p>
          )}
          {suggestions.map((s) => (
            <div key={s.id} className="rounded-md border p-3 space-y-2 text-sm">
              <div className="flex items-start justify-between gap-2">
                <div className="flex-1 min-w-0">
                  <div className="font-medium truncate">{s.representative_goal}</div>
                  <div className="text-xs text-muted-foreground">
                    {s.size} similar runs · sha:{s.id}
                  </div>
                  {s.common_tokens && s.common_tokens.length > 0 && (
                    <div className="mt-1 flex flex-wrap gap-1">
                      {s.common_tokens.slice(0, 8).map((t) => (
                        <Badge key={t} variant="secondary" className="text-xs">
                          {t}
                        </Badge>
                      ))}
                    </div>
                  )}
                </div>
                {promoteFor !== s.id ? (
                  <div className="flex gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => void synthesize(s)}
                      disabled={synthesizing === s.id}
                      title="Use the LLM to draft a reusable system prompt + capabilities from this cluster"
                    >
                      {synthesizing === s.id ? "Synthesizing…" : "Generate with AI"}
                    </Button>
                    <Button
                      size="sm"
                      onClick={() => {
                        setPromoteFor(s.id);
                        setPromoteName(s.common_tokens?.slice(0, 3).join(" ") || "");
                        setSynthesis(null);
                        setSynthesisFor(null);
                      }}
                    >
                      Promote
                    </Button>
                  </div>
                ) : null}
              </div>
              {promoteFor === s.id && (
                <div className="space-y-2 border-t pt-2">
                  {synthesisFor === s.id && synthesis && (
                    <div className="rounded-md border bg-muted/50 p-2 space-y-1 text-xs">
                      <div className="font-medium">AI-generated draft</div>
                      {synthesis.explanation && (
                        <div className="text-muted-foreground">{synthesis.explanation}</div>
                      )}
                      <div>
                        <span className="text-muted-foreground">Role:</span>{" "}
                        {synthesis.suggested_role || "—"}
                      </div>
                      <div>
                        <span className="text-muted-foreground">Capabilities:</span>{" "}
                        {synthesis.capabilities.join(", ")}
                      </div>
                      <details>
                        <summary className="cursor-pointer text-muted-foreground">
                          System prompt
                        </summary>
                        <pre className="mt-1 whitespace-pre-wrap text-xs">
                          {synthesis.system_prompt}
                        </pre>
                      </details>
                    </div>
                  )}
                  <Input
                    placeholder="Name for the new skill"
                    value={promoteName}
                    onChange={(e) => setPromoteName(e.target.value)}
                  />
                  <div className="flex justify-end gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        setPromoteFor(null);
                        setPromoteName("");
                        setSynthesis(null);
                        setSynthesisFor(null);
                      }}
                    >
                      Cancel
                    </Button>
                    <Button size="sm" onClick={() => void promote(s)}>
                      Create recipe + assistant
                    </Button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </CardContent>
      </Card>

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
