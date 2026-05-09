import { useEffect, useMemo, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { assistantsApi, pluginsApi, providersApi, runsApi, settingsApi } from "@/lib/api";
import type { Assistant, Plugin, ProviderProfileRequest } from "@/types/api";
import { OutcomeConnectorPicker } from "./outcome-connectors";

type VerifyState = "idle" | "running" | "success" | "failed" | "stuck";

// 30 ticks × 2s ≈ 1 minute upper bound. After that we present a "Continue
// anyway" path because the user shouldn't be trapped in the wizard if a
// remote model is just slow. Local Ollama responds in seconds; remote
// models on cold cache can take longer.
const VERIFY_MAX_TICKS = 30;
const VERIFY_TICK_MS = 2000;

type ProviderChoice = "ollama" | "anthropic" | "openai";

async function checkOllamaReachable(): Promise<boolean> {
  try {
    return await invoke<boolean>("check_ollama_reachable");
  } catch {
    try {
      const res = await fetch("http://127.0.0.1:11434/api/tags");
      return res.ok;
    } catch {
      return false;
    }
  }
}

// pickOllamaModel queries Ollama for installed models and returns the most
// sensible default. Falls back to a hard-coded "qwen2.5:latest" only when
// the API is unreachable; the caller will get a 404 the first time it tries
// to chat if that model isn't actually installed, which is at least a clear
// error to fix in Settings → AI Providers.
//
// We DON'T use the assistant template's suggested_model here. Templates
// suggest remote models (e.g. claude-3-5-sonnet) tuned for capability, not
// what's locally installed; piping that into an Ollama profile would 404
// every chat with no actionable hint.
async function pickOllamaModel(): Promise<string> {
  const fallback = "qwen2.5:latest";
  try {
    const res = await fetch("http://127.0.0.1:11434/api/tags");
    if (!res.ok) return fallback;
    const data = (await res.json()) as { models?: Array<{ name: string }> };
    const names = (data.models ?? []).map((m) => m.name);
    if (names.length === 0) return fallback;
    // Prefer chat-capable, well-rounded models in this order, then any
    // installed model as a last resort.
    const preferred = ["qwen2.5", "llama3.2", "llama3.1", "mistral", "qwen2"];
    for (const stem of preferred) {
      const hit = names.find((n) => n === `${stem}:latest`) ?? names.find((n) => n.startsWith(`${stem}:`));
      if (hit) return hit;
    }
    return names[0];
  } catch {
    return fallback;
  }
}

async function pickWorkspaceFolder(initialPath: string): Promise<string | null> {
  try {
    const selected = await invoke<string | null>("pick_workspace_folder", {
      initialPath,
    });
    return selected || null;
  } catch {
    return null;
  }
}

export function OnboardingWizard({
  templates,
  onComplete,
  onSkip,
}: {
  templates: Assistant[];
  onComplete: () => void;
  onSkip: () => void | Promise<void>;
}) {
  const [step, setStep] = useState(1);
  // Quickstart collapses the 5-step wizard into a single-screen form
  // (Code Reviewer + a provider + workspace) for the median user, who
  // wants to run their first task in under two minutes. The full wizard
  // stays available behind "Show all options" for users who actually
  // want to pick a different template, configure connectors, etc.
  const [mode, setMode] = useState<"quickstart" | "wizard">("quickstart");
  const [selectedTemplateId, setSelectedTemplateId] = useState<string>(() => {
    const reviewer = templates.find((t) => t.template_id === "code-reviewer");
    return (reviewer ?? templates[0])?.id || "";
  });
  // Anthropic is the default fast-path because it ships an API key that
  // works against current frontier models without a separate model
  // download. Users without an Anthropic key flip to OpenAI or Ollama.
  const [providerChoice, setProviderChoice] = useState<ProviderChoice>("anthropic");
  const [anthropicKey, setAnthropicKey] = useState("");
  const [openaiKey, setOpenaiKey] = useState("");
  const [workspacePath, setWorkspacePath] = useState(".");
  const [checkingOllama, setCheckingOllama] = useState(false);
  const [ollamaReachable, setOllamaReachable] = useState<boolean | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Step 5 — outcome-first connector flow. Fetched lazily when the user
  // reaches this step so we don't block the wizard on slow plugin list.
  const [plugins, setPlugins] = useState<Plugin[]>([]);
  const [pluginsLoading, setPluginsLoading] = useState(false);

  // Step 4 — first-task verification. The wizard already creates a Run as
  // the user's first conversation; we now poll it and tell the user
  // whether the LLM round-trip actually worked before dropping them into
  // the main app. This catches "wrong API key", "Ollama model not
  // pulled", capability-ceiling violations, etc. up front.
  const [verifyState, setVerifyState] = useState<VerifyState>("idle");
  const [verifyMessage, setVerifyMessage] = useState<string>("");
  const [verifyAssistantName, setVerifyAssistantName] = useState<string>("");
  const verifyCancelled = useRef<boolean>(false);

  const selectedTemplate = useMemo(
    () => templates.find((t) => t.id === selectedTemplateId) || templates[0],
    [templates, selectedTemplateId],
  );

  const canContinueProvider =
    providerChoice === "ollama" ||
    (providerChoice === "anthropic" ? anthropicKey.trim().length > 0 : openaiKey.trim().length > 0);

  // handleSkip auto-creates a Code Reviewer assistant pointed at the
  // current folder before exiting so the user lands in Chats with a
  // selectable assistant + working starter prompts instead of an empty
  // dropdown. Provider can be added later in Settings → AI Providers;
  // assistant creation does not require one.
  const handleSkip = async () => {
    try {
      const existing = await assistantsApi.list();
      if (existing.assistants.length === 0) {
        const reviewer =
          templates.find((t) => t.template_id === "code-reviewer") ?? templates[0];
        if (reviewer) {
          await assistantsApi.create({
            template_id: reviewer.template_id,
            name: reviewer.name,
            tagline: reviewer.tagline,
            role: reviewer.role,
            best_for: reviewer.best_for,
            not_for: reviewer.not_for,
            suggested_model: reviewer.suggested_model,
            system_prompt: reviewer.system_prompt,
            channels: reviewer.channels,
            channel_configs: reviewer.channel_configs,
            capabilities: reviewer.capabilities,
            contexts: [{ type: "folder", path: "." }],
            memory_policy: reviewer.memory_policy,
            permission_policy: reviewer.permission_policy,
            model_policy: reviewer.model_policy,
          });
        }
      }
    } catch {
      // Non-fatal — falls back to original empty-chat behavior.
    }
    void onSkip();
  };

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "w") {
        event.preventDefault();
        void handleSkip();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- handleSkip closes over stable templates + onSkip
  }, [onSkip, templates]);

  const runOllamaCheck = async () => {
    setCheckingOllama(true);
    const ok = await checkOllamaReachable();
    setOllamaReachable(ok);
    setCheckingOllama(false);
  };

  const providerPayload = async (): Promise<ProviderProfileRequest> => {
    if (providerChoice === "ollama") {
      // Use a model Ollama actually has installed. The template's
      // suggested_model is a remote model name (e.g. claude-3-5-sonnet)
      // that Ollama would 404 on, so we ignore it for local providers.
      const model = await pickOllamaModel();
      return {
        name: "Ollama (Local)",
        type: "local",
        endpoint: "http://127.0.0.1:11434",
        model_ids: [model],
        enabled: true,
      };
    }
    if (providerChoice === "anthropic") {
      // Anthropic adapter posts to baseURL + "/messages"; pass the API
      // root, not the full path, or the request becomes /v1/messages/messages.
      return {
        name: "Anthropic",
        type: "remote",
        endpoint: "https://api.anthropic.com/v1",
        model_ids: ["claude-3-5-sonnet"],
        secret_ref: anthropicKey.trim(),
        enabled: true,
      };
    }
    // OpenAI adapter posts to baseURL + "/chat/completions"; same rule.
    return {
      name: "OpenAI",
      type: "remote",
      endpoint: "https://api.openai.com/v1",
      model_ids: ["gpt-4o-mini"],
      secret_ref: openaiKey.trim(),
      enabled: true,
    };
  };

  const handleFinish = async () => {
    if (!selectedTemplate) return;
    setSubmitting(true);
    setError(null);
    try {
      const provider = await providersApi.create(await providerPayload());
      const modelID = provider.model_ids[0] || "";
      if (modelID) {
        await settingsApi.setLLMDefault({ provider_id: provider.id, model_id: modelID });
      }

      const createdAssistant = await assistantsApi.create({
        template_id: selectedTemplate.template_id,
        name: selectedTemplate.name,
        tagline: selectedTemplate.tagline,
        role: selectedTemplate.role,
        best_for: selectedTemplate.best_for,
        not_for: selectedTemplate.not_for,
        suggested_model: selectedTemplate.suggested_model,
        system_prompt: selectedTemplate.system_prompt,
        channels: selectedTemplate.channels,
        channel_configs: selectedTemplate.channel_configs,
        capabilities: selectedTemplate.capabilities,
        contexts: [{ type: "folder", path: workspacePath.trim() || "." }],
        memory_policy: selectedTemplate.memory_policy,
        permission_policy: selectedTemplate.permission_policy,
        model_policy: selectedTemplate.model_policy,
      });

      const run = await runsApi.create({
        assistant_id: createdAssistant.id,
        goal: "Help me get started: ask what I want to accomplish and propose a first plan.",
      });

      // Don't mark onboarding complete yet — that happens only on a
      // verified-good first run (handled in pollVerification) or when
      // the user takes an explicit Continue-anyway / Skip path. Setting
      // it here would land users with broken provider/model setups in a
      // state where the wizard never re-opens.
      //
      // Advance to step 4 and poll the run so we can show the user a
      // real "ready / failed / still warming up" signal.
      setVerifyAssistantName(createdAssistant.name);
      setVerifyState("running");
      setVerifyMessage("Asking " + createdAssistant.name + " to introduce themselves…");
      setStep(4);
      verifyCancelled.current = false;
      void pollVerification(run.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  // Poll the verification run until it lands in a terminal-or-meaningful
  // state. plan_review and awaiting_approval count as "ready" — Nomi got
  // far enough to plan or call a tool, the user just needs to step in.
  const pollVerification = async (runId: string) => {
    for (let i = 0; i < VERIFY_MAX_TICKS; i++) {
      if (verifyCancelled.current) return;
      await new Promise((resolve) => setTimeout(resolve, VERIFY_TICK_MS));
      if (verifyCancelled.current) return;

      try {
        const detail = await runsApi.get(runId);
        const status = detail.run.status;

        if (status === "completed") {
          setVerifyState("success");
          setVerifyMessage(verifyAssistantName + " replied. You're all set.");
          // Verified-good first run: it's now safe to mark onboarding complete.
          void settingsApi.setOnboardingComplete(true).catch(() => {});
          return;
        }
        if (status === "plan_review" || status === "awaiting_approval") {
          // Got to planning/approval = LLM works, permissions work,
          // permission engine works. Treat as ready.
          setVerifyState("success");
          setVerifyMessage(
            verifyAssistantName +
              " prepared a first step that needs your review. Continue to Nomi to approve it.",
          );
          void settingsApi.setOnboardingComplete(true).catch(() => {});
          return;
        }
        if (status === "failed" || status === "cancelled") {
          setVerifyState("failed");
          // Step-level error message is the actionable one — the run
          // itself doesn't carry an error field on the wire (the `Run`
          // type in @/types/api intentionally only carries lifecycle
          // metadata; details live on steps).
          const stepError = detail.steps?.find((s) => s.error)?.error;
          setVerifyMessage(stepError || "Verification run did not complete successfully.");
          return;
        }
        // created / planning / executing → keep polling.
      } catch (err) {
        // Transient (e.g. SSE/HTTP race after restart) — keep polling
        // until the budget runs out. Surface the last error if we time
        // out so the user has something to act on.
        setVerifyMessage(
          "Still checking… (" +
            (err instanceof Error ? err.message : String(err)) +
            ")",
        );
      }
    }
    setVerifyState("stuck");
    setVerifyMessage(
      "Nomi is taking longer than expected. You can continue and watch progress in Chats, or go back to reconfigure.",
    );
  };

  const cancelVerificationAndExit = () => {
    verifyCancelled.current = true;
    // Continue-anyway is an explicit user choice — record the intent
    // even though verification didn't pass, so the wizard doesn't
    // re-open on next launch. Without this we'd punish the user for
    // making a deliberate decision.
    void settingsApi.setOnboardingComplete(true).catch(() => {});
    onComplete();
  };

  const advanceToConnectors = () => {
    verifyCancelled.current = true;
    // Reaching the connector picker means verification produced an
    // actionable state (success/stuck) and the user opted in. Lock
    // onboarding-complete here so a refresh mid-step-5 doesn't reopen
    // the wizard.
    void settingsApi.setOnboardingComplete(true).catch(() => {});
    setStep(5);
    setPluginsLoading(true);
    pluginsApi
      .list()
      .then((data) => setPlugins(data.plugins))
      .catch(() => setPlugins([]))
      .finally(() => setPluginsLoading(false));
  };

  const reconfigureFromStep4 = () => {
    // Reconfigure must NOT mark onboarding complete: user is rolling
    // back to fix a broken setup. Leaving the flag unset means a fresh
    // app launch reopens the wizard at the same place.
    verifyCancelled.current = true;
    setVerifyState("idle");
    setVerifyMessage("");
    setStep(2);
  };

  return (
    <div className="min-h-screen bg-background text-foreground flex items-center justify-center p-6">
      <Card className="w-full max-w-4xl">
        <CardHeader>
          <div className="flex items-center justify-between gap-4">
            <div>
              <CardTitle className="text-2xl">Welcome to Nomi</CardTitle>
              <p className="text-sm text-muted-foreground mt-1">Get set up in under a minute.</p>
            </div>
            <Badge variant="outline">
              {mode === "quickstart"
                ? step === 1
                  ? "Setup"
                  : "Verify"
                : `Step ${step} of 5`}
            </Badge>
          </div>
          {mode === "wizard" && (
            <ol
              aria-label="Onboarding progress"
              className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground mt-3"
            >
              {([
                "Template",
                "Provider",
                "Workspace",
                "Verify",
                "Connect",
              ] as const).map((label, idx) => {
                const n = idx + 1;
                const state = step === n ? "current" : step > n ? "done" : "upcoming";
                return (
                  <li
                    key={label}
                    aria-current={state === "current" ? "step" : undefined}
                    className={
                      state === "current"
                        ? "font-medium text-foreground"
                        : state === "done"
                          ? "text-foreground/70"
                          : "text-muted-foreground/60"
                    }
                  >
                    <span aria-hidden="true" className="mr-1">
                      {state === "done" ? "✓" : n}.
                    </span>
                    {label}
                  </li>
                );
              })}
            </ol>
          )}
        </CardHeader>
        <CardContent className="space-y-6">
          {step === 1 && mode === "quickstart" && (
            <div className="space-y-4">
              <div className="space-y-1">
                <h2 className="font-medium">Quick start</h2>
                <p className="text-sm text-muted-foreground">
                  Code Reviewer pointed at your current folder. Pick a provider, paste a key, and you&apos;re done.
                </p>
              </div>

              <div className="grid grid-cols-1 md:grid-cols-3 gap-2">
                {[
                  { id: "anthropic", title: "Anthropic", desc: "Frontier model, no install" },
                  { id: "openai", title: "OpenAI", desc: "Frontier model, no install" },
                  { id: "ollama", title: "Ollama (local)", desc: "Free, private, requires install" },
                ].map((opt) => (
                  <button
                    key={opt.id}
                    type="button"
                    onClick={() => setProviderChoice(opt.id as ProviderChoice)}
                    className={`text-left rounded-md border p-3 transition-colors hover:bg-muted/40 ${
                      providerChoice === opt.id ? "border-primary bg-primary/5" : "border-border"
                    }`}
                  >
                    <p className="font-medium text-sm">{opt.title}</p>
                    <p className="text-xs text-muted-foreground mt-1">{opt.desc}</p>
                  </button>
                ))}
              </div>

              {providerChoice === "anthropic" && (
                <div className="space-y-1">
                  <label htmlFor="qs-anthropic-key" className="text-sm font-medium">
                    Anthropic API key
                  </label>
                  <Input
                    id="qs-anthropic-key"
                    type="password"
                    autoComplete="off"
                    value={anthropicKey}
                    onChange={(e) => setAnthropicKey(e.target.value)}
                    placeholder="sk-ant-..."
                  />
                </div>
              )}

              {providerChoice === "openai" && (
                <div className="space-y-1">
                  <label htmlFor="qs-openai-key" className="text-sm font-medium">
                    OpenAI API key
                  </label>
                  <Input
                    id="qs-openai-key"
                    type="password"
                    autoComplete="off"
                    value={openaiKey}
                    onChange={(e) => setOpenaiKey(e.target.value)}
                    placeholder="sk-..."
                  />
                </div>
              )}

              {providerChoice === "ollama" && (
                <div className="rounded-md border p-3 space-y-2">
                  <p className="text-sm">Ollama runs the model on your machine. Detected on the first try; install at <a href="https://ollama.ai" rel="noopener" className="underline">ollama.ai</a> if you don&apos;t have it.</p>
                  <div className="flex items-center gap-2">
                    <Button type="button" variant="outline" onClick={runOllamaCheck} disabled={checkingOllama}>
                      {checkingOllama ? "Checking..." : "Check Ollama"}
                    </Button>
                    {ollamaReachable === true && <Badge>Detected</Badge>}
                    {ollamaReachable === false && <Badge variant="destructive">Not detected</Badge>}
                  </div>
                </div>
              )}

              <div className="space-y-1">
                <label htmlFor="qs-workspace" className="text-sm font-medium">
                  Workspace folder
                </label>
                <div className="flex gap-2">
                  <Input
                    id="qs-workspace"
                    value={workspacePath}
                    onChange={(e) => setWorkspacePath(e.target.value)}
                    placeholder="/path/to/your/repo"
                  />
                  <Button
                    type="button"
                    variant="outline"
                    onClick={async () => {
                      const selected = await pickWorkspaceFolder(workspacePath);
                      if (selected) setWorkspacePath(selected);
                    }}
                  >
                    Pick folder
                  </Button>
                </div>
                <p className="text-xs text-muted-foreground">
                  Code Reviewer can only read and propose changes inside this folder.
                </p>
              </div>

              <div className="pt-1">
                <button
                  type="button"
                  onClick={() => setMode("wizard")}
                  className="text-xs text-muted-foreground hover:underline"
                >
                  Show all options (different template, connectors, …)
                </button>
              </div>
            </div>
          )}

          {step === 1 && mode === "wizard" && (
            <div className="space-y-3">
              <h2 className="font-medium">What would you like help with?</h2>
              <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                {templates.map((tpl) => (
                  <button
                    key={tpl.id}
                    type="button"
                    onClick={() => setSelectedTemplateId(tpl.id)}
                    className={`text-left rounded-md border p-3 transition-colors hover:bg-muted/40 ${
                      selectedTemplateId === tpl.id ? "border-primary bg-primary/5" : "border-border"
                    }`}
                  >
                    <p className="font-medium text-sm">{tpl.name}</p>
                    {tpl.tagline && <p className="text-xs text-muted-foreground mt-1">{tpl.tagline}</p>}
                    <p className="text-xs mt-2">
                      <span className="font-medium">Best for:</span> {tpl.best_for}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      <span className="font-medium text-foreground">Not for:</span> {tpl.not_for}
                    </p>
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={() => setMode("quickstart")}
                className="text-xs text-muted-foreground hover:underline"
              >
                ← Back to quick start
              </button>
            </div>
          )}

          {step === 2 && (
            <div className="space-y-4">
              <h2 className="font-medium">Where should Nomi think?</h2>
              <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
                {[
                  { id: "ollama", title: "Use Ollama", desc: "Free, private, local" },
                  { id: "anthropic", title: "Use Anthropic", desc: "High-quality remote model" },
                  { id: "openai", title: "Use OpenAI", desc: "Fast remote model" },
                ].map((opt) => (
                  <button
                    key={opt.id}
                    type="button"
                    onClick={() => setProviderChoice(opt.id as ProviderChoice)}
                    className={`text-left rounded-md border p-3 transition-colors hover:bg-muted/40 ${
                      providerChoice === opt.id ? "border-primary bg-primary/5" : "border-border"
                    }`}
                  >
                    <p className="font-medium text-sm">{opt.title}</p>
                    <p className="text-xs text-muted-foreground mt-1">{opt.desc}</p>
                  </button>
                ))}
              </div>

              {providerChoice === "ollama" && (
                <div className="rounded-md border p-3 space-y-2">
                  <p className="text-sm">Check local Ollama availability.</p>
                  <div className="flex items-center gap-2">
                    <Button type="button" variant="outline" onClick={runOllamaCheck} disabled={checkingOllama}>
                      {checkingOllama ? "Checking..." : "Check Ollama"}
                    </Button>
                    {ollamaReachable === true && <Badge>Detected</Badge>}
                    {ollamaReachable === false && <Badge variant="destructive">Not detected</Badge>}
                  </div>
                  {ollamaReachable === false && (
                    <p className="text-xs text-muted-foreground">
                      Install/start Ollama, then retry. Nomi expects it on <code>localhost:11434</code>.
                    </p>
                  )}
                </div>
              )}

              {providerChoice === "anthropic" && (
                <div className="space-y-2">
                  <label className="text-sm font-medium">Anthropic API key</label>
                  <Input
                    type="password"
                    value={anthropicKey}
                    onChange={(e) => setAnthropicKey(e.target.value)}
                    placeholder="sk-ant-..."
                  />
                </div>
              )}

              {providerChoice === "openai" && (
                <div className="space-y-2">
                  <label className="text-sm font-medium">OpenAI API key</label>
                  <Input
                    type="password"
                    value={openaiKey}
                    onChange={(e) => setOpenaiKey(e.target.value)}
                    placeholder="sk-..."
                  />
                </div>
              )}
            </div>
          )}

          {step === 3 && (
            <div className="space-y-3">
              <h2 className="font-medium">What can Nomi see?</h2>
              <p className="text-sm text-muted-foreground">Set the workspace folder boundary for this assistant.</p>
              <div className="flex gap-2">
                <Input
                  value={workspacePath}
                  onChange={(e) => setWorkspacePath(e.target.value)}
                  placeholder="/path/to/workspace"
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={async () => {
                    const selected = await pickWorkspaceFolder(workspacePath);
                    if (selected) setWorkspacePath(selected);
                  }}
                >
                  Pick folder
                </Button>
              </div>
            </div>
          )}

          {step === 4 && (
            <div className="space-y-4">
              <h2 className="font-medium">Checking the connection</h2>

              {verifyState === "running" && (
                <div className="rounded-md border p-4 space-y-2">
                  <div className="flex items-center gap-3">
                    <span
                      className="inline-block h-3 w-3 rounded-full bg-primary animate-pulse"
                      aria-hidden
                    />
                    <p className="text-sm">{verifyMessage}</p>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    This usually takes a few seconds — longer for the first request to a remote model.
                  </p>
                </div>
              )}

              {verifyState === "success" && (
                <div className="rounded-md border border-emerald-300 bg-emerald-50 dark:border-emerald-700 dark:bg-emerald-950 p-4 space-y-2">
                  <p className="text-sm font-medium text-emerald-900 dark:text-emerald-200">
                    Nomi is ready
                  </p>
                  <p className="text-sm text-emerald-900 dark:text-emerald-200">{verifyMessage}</p>
                </div>
              )}

              {verifyState === "failed" && (
                <div className="rounded-md border border-red-300 bg-red-50 dark:border-red-700 dark:bg-red-950 p-4 space-y-3">
                  <p className="text-sm font-medium text-red-900 dark:text-red-200">
                    Nomi couldn&apos;t complete a first reply
                  </p>
                  <p className="text-sm text-red-900 dark:text-red-200 break-words">
                    {verifyMessage}
                  </p>
                  <p className="text-xs text-red-900/80 dark:text-red-200/80">
                    Common causes: wrong API key, model not available on the chosen provider, or
                    the local Ollama model wasn&apos;t pulled. Reconfigure to fix the provider step.
                  </p>
                </div>
              )}

              {verifyState === "stuck" && (
                <div className="rounded-md border border-amber-300 bg-amber-50 dark:border-amber-700 dark:bg-amber-950 p-4 space-y-2">
                  <p className="text-sm font-medium text-amber-900 dark:text-amber-200">
                    Still warming up
                  </p>
                  <p className="text-sm text-amber-900 dark:text-amber-200">{verifyMessage}</p>
                </div>
              )}
            </div>
          )}

          {error && <p className="text-sm text-destructive">{error}</p>}

          {step !== 5 && (
            <div className="flex items-center justify-between pt-2">
              <Button
                type="button"
                variant="ghost"
                onClick={() => void handleSkip()}
                disabled={submitting || verifyState === "running"}
              >
                Skip for now
              </Button>

              <div className="flex gap-2">
                {step > 1 && step < 4 && mode === "wizard" && (
                  <Button type="button" variant="outline" onClick={() => setStep((s) => s - 1)} disabled={submitting}>
                    Back
                  </Button>
                )}
                {step === 1 && mode === "quickstart" && (
                  <Button
                    type="button"
                    onClick={handleFinish}
                    disabled={submitting || !canContinueProvider || !workspacePath.trim()}
                    autoFocus
                  >
                    {submitting ? "Starting..." : "Start"}
                  </Button>
                )}
                {step < 3 && mode === "wizard" && (
                  <Button
                    type="button"
                    onClick={() => setStep((s) => s + 1)}
                    disabled={step === 2 && !canContinueProvider}
                  >
                    Continue
                  </Button>
                )}
                {step === 3 && mode === "wizard" && (
                  <Button type="button" onClick={handleFinish} disabled={submitting || !workspacePath.trim()}>
                    {submitting ? "Creating..." : "Get started"}
                  </Button>
                )}
                {step === 4 && verifyState === "running" && (
                  <Button type="button" variant="outline" onClick={cancelVerificationAndExit}>
                    Continue anyway
                  </Button>
                )}
                {step === 4 && verifyState === "success" && (
                  <Button type="button" onClick={advanceToConnectors}>
                    Continue
                  </Button>
                )}
                {step === 4 && verifyState === "failed" && (
                  <>
                    <button
                      type="button"
                      onClick={cancelVerificationAndExit}
                      className="text-xs text-muted-foreground underline-offset-2 hover:underline self-center"
                    >
                      Continue anyway
                    </button>
                    <Button type="button" onClick={reconfigureFromStep4} autoFocus>
                      Reconfigure
                    </Button>
                  </>
                )}
                {step === 4 && verifyState === "stuck" && (
                  <>
                    <Button type="button" variant="outline" onClick={reconfigureFromStep4}>
                      Reconfigure
                    </Button>
                    <Button type="button" onClick={advanceToConnectors}>
                      Continue
                    </Button>
                  </>
                )}
              </div>
            </div>
          )}

          {step === 5 && (
            <>
              {pluginsLoading ? (
                <div className="flex items-center justify-center py-12">
                  <div className="flex items-center gap-2 text-sm text-muted-foreground">
                    <span className="inline-block h-4 w-4 rounded-full border-2 border-primary border-t-transparent animate-spin" />
                    Loading connectors…
                  </div>
                </div>
              ) : (
                <OutcomeConnectorPicker
                  plugins={plugins}
                  recommendedPluginIds={selectedTemplate?.recommended_bindings?.map((b) => b.plugin_id) ?? []}
                  onSkip={cancelVerificationAndExit}
                  onDone={cancelVerificationAndExit}
                  mode="wizard"
                />
              )}
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
