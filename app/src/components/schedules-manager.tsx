import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { ToggleSwitch } from "@/components/ui/toggle-switch";
import { assistantsApi, schedulesApi, type Schedule, type TranslateResult } from "@/lib/api";
import type { Assistant } from "@/types/api";

// Schedules tab. Cron-driven Runs against a chosen assistant — the
// NL phrase input calls /schedules/translate so users don't need to
// remember cron syntax. After the translator returns, the parsed cron
// + the LLM's explanation are surfaced for confirmation before save.
export function SchedulesManager() {
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [assistants, setAssistants] = useState<Assistant[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Create form state.
  const [phrase, setPhrase] = useState("");
  const [assistantID, setAssistantID] = useState("");
  const [prompt, setPrompt] = useState("");
  const [translating, setTranslating] = useState(false);
  const [translation, setTranslation] = useState<TranslateResult | null>(null);
  const [saving, setSaving] = useState(false);

  const refresh = async () => {
    try {
      const [s, a] = await Promise.all([schedulesApi.list(), assistantsApi.list()]);
      setSchedules(s.schedules);
      setAssistants(a.assistants);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
  }, []);

  const translate = async () => {
    if (!phrase.trim()) return;
    setTranslating(true);
    setError(null);
    try {
      const result = await schedulesApi.translate(phrase.trim());
      setTranslation(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setTranslating(false);
    }
  };

  const save = async () => {
    if (!translation || !translation.valid) return;
    if (!assistantID || !prompt.trim()) {
      setError("Choose an assistant and write a prompt before saving.");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await schedulesApi.create({
        assistant_id: assistantID,
        prompt: prompt.trim(),
        cron_expr: translation.cron_expr,
        nl_phrase: translation.nl_phrase,
      });
      setPhrase("");
      setPrompt("");
      setTranslation(null);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const toggle = async (s: Schedule) => {
    try {
      await schedulesApi.patch(s.id, { enabled: !s.enabled });
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const remove = async (s: Schedule) => {
    if (!confirm(`Delete schedule "${s.prompt.slice(0, 40)}…"?`)) return;
    try {
      await schedulesApi.delete(s.id);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  if (loading) {
    return <div className="p-4 text-sm text-muted-foreground">Loading schedules…</div>;
  }

  return (
    <div className="space-y-4 p-4">
      <div>
        <h2 className="text-lg font-semibold">Schedules</h2>
        <p className="text-sm text-muted-foreground">
          Fire a Run on a recurring cadence. Type a phrase like{" "}
          <em>&ldquo;every weekday at 8am&rdquo;</em> — Nomi translates it to cron via your default LLM
          provider, then runs the prompt against the chosen assistant on schedule.
        </p>
      </div>

      {error && (
        <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm">
          {error}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">New schedule</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">When</label>
            <div className="flex gap-2">
              <Input
                placeholder="e.g. every weekday at 8am"
                value={phrase}
                onChange={(e) => setPhrase(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void translate();
                }}
              />
              <Button onClick={() => void translate()} disabled={translating || !phrase.trim()}>
                {translating ? "Translating…" : "Translate"}
              </Button>
            </div>
          </div>

          {translation && (
            <div
              className={
                "rounded-md border p-3 text-sm space-y-1 " +
                (translation.valid
                  ? "border-green-500/40 bg-green-500/10"
                  : "border-amber-500/40 bg-amber-500/10")
              }
            >
              {translation.valid ? (
                <>
                  <div>
                    Cron:{" "}
                    <code className="font-mono">{translation.cron_expr}</code>
                  </div>
                  <div className="text-muted-foreground">{translation.explanation}</div>
                </>
              ) : (
                <div>
                  <strong>Can&apos;t translate:</strong> {translation.explanation || "unknown error"}
                </div>
              )}
            </div>
          )}

          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">Assistant</label>
            <select
              className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
              value={assistantID}
              onChange={(e) => setAssistantID(e.target.value)}
            >
              <option value="">Choose an assistant…</option>
              {assistants.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </div>

          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">Prompt</label>
            <Input
              placeholder="What should the assistant do each time?"
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
            />
          </div>

          <div className="flex justify-end">
            <Button
              onClick={() => void save()}
              disabled={saving || !translation?.valid || !assistantID || !prompt.trim()}
            >
              {saving ? "Saving…" : "Save schedule"}
            </Button>
          </div>
        </CardContent>
      </Card>

      <div>
        <h3 className="text-sm font-medium mb-2">Active schedules ({schedules.length})</h3>
        {schedules.length === 0 && (
          <p className="text-sm text-muted-foreground">No schedules yet.</p>
        )}
        <div className="space-y-2">
          {schedules.map((s) => {
            const assistant = assistants.find((a) => a.id === s.assistant_id);
            return (
              <Card key={s.id}>
                <CardContent className="p-3 space-y-1">
                  <div className="flex items-start justify-between gap-2">
                    <div className="flex-1 min-w-0">
                      <div className="font-medium truncate">{s.prompt}</div>
                      <div className="text-xs text-muted-foreground">
                        {assistant ? assistant.name : s.assistant_id}
                        {" · "}
                        <code className="font-mono">{s.cron_expr}</code>
                        {s.nl_phrase && ` · "${s.nl_phrase}"`}
                      </div>
                      <div className="text-xs text-muted-foreground">
                        Next fire: {new Date(s.next_fire_at).toLocaleString()}
                        {s.last_fire_at &&
                          ` · last fire: ${new Date(s.last_fire_at).toLocaleString()}`}
                      </div>
                      {s.last_error && (
                        <div className="text-xs text-destructive">Last error: {s.last_error}</div>
                      )}
                    </div>
                    <div className="flex items-center gap-2">
                      <Badge variant={s.enabled ? "default" : "outline"}>
                        {s.enabled ? "Enabled" : "Disabled"}
                      </Badge>
                      <ToggleSwitch checked={s.enabled} onChange={() => void toggle(s)} />
                      <Button size="sm" variant="outline" onClick={() => void remove(s)}>
                        Delete
                      </Button>
                    </div>
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </div>
      </div>
    </div>
  );
}
