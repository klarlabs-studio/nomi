import { useState, useEffect } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
  DialogFooter,
} from "@/components/ui/dialog";
import { providersApi, settingsApi } from "@/lib/api";
import type {
  ProviderProfile,
  ProviderProfileRequest,
  LLMDefaultSettings,
} from "@/types/api";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import { Eye, EyeOff, X } from "lucide-react";

interface ProviderPreset {
  name: string;
  type: "local" | "remote";
  endpoint: string;
  models: string[];
}

const PROVIDER_PRESETS: ProviderPreset[] = [
  { name: "OpenAI", type: "remote", endpoint: "https://api.openai.com/v1", models: ["gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"] },
  { name: "Anthropic", type: "remote", endpoint: "https://api.anthropic.com/v1", models: ["claude-3-5-sonnet-20241022", "claude-3-opus-20240229", "claude-3-haiku-20240307"] },
  {
    name: "OpenRouter",
    type: "remote",
    endpoint: "https://openrouter.ai/api/v1",
    models: [
      "openai/gpt-4o",
      "anthropic/claude-sonnet-4",
      "google/gemini-2.5-flash",
      "meta-llama/llama-3.3-70b-instruct",
    ],
  },
  { name: "Google (Gemini)", type: "remote", endpoint: "https://generativelanguage.googleapis.com/v1", models: ["gemini-1.5-pro", "gemini-1.5-flash", "gemini-1.0-pro"] },
  { name: "AWS Bedrock", type: "remote", endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com", models: ["anthropic.claude-3-5-sonnet", "amazon.titan-text-express", "meta.llama3-1-70b"] },
  { name: "Azure OpenAI", type: "remote", endpoint: "https://your-resource.openai.azure.com/openai/deployments", models: ["gpt-4o", "gpt-4", "gpt-35-turbo"] },
  { name: "Groq", type: "remote", endpoint: "https://api.groq.com/openai/v1", models: ["llama-3.1-70b-versatile", "mixtral-8x7b-32768", "gemma-7b-it"] },
  { name: "Ollama", type: "local", endpoint: "http://localhost:11434/v1", models: ["llama3.1", "llama3.2", "mistral", "codellama", "phi3", "gemma2"] },
  { name: "LM Studio", type: "local", endpoint: "http://localhost:1234/v1", models: ["local-model"] },
];

const DEFAULT_ENDPOINTS = {
  remote: "https://api.openai.com/v1",
  local: "http://localhost:11434/v1",
};

const MODEL_CATALOG: Record<string, { category: string; models: string[] }[]> = {
  remote: [
    { category: "OpenAI", models: ["gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"] },
    { category: "Anthropic", models: ["claude-3-5-sonnet", "claude-3-opus", "claude-3-haiku"] },
    {
      category: "OpenRouter",
      models: [
        "openai/gpt-4o",
        "anthropic/claude-sonnet-4",
        "google/gemini-2.5-flash",
        "meta-llama/llama-3.3-70b-instruct",
      ],
    },
    { category: "Google", models: ["gemini-1.5-pro", "gemini-1.5-flash"] },
    { category: "Meta (Llama)", models: ["llama3.1", "llama3.2"] },
    { category: "Mistral", models: ["mistral", "codellama"] },
  ],
  local: [
    { category: "Meta (Llama)", models: ["llama3.1", "llama3.2"] },
    { category: "Mistral", models: ["mistral", "codellama"] },
    { category: "Microsoft", models: ["phi3", "phi4"] },
    { category: "Google", models: ["gemma2"] },
    { category: "Alibaba", models: ["qwen2.5"] },
  ],
};

function ProviderCard({
  profile,
  isDefault,
  onEdit,
  onDelete,
  onSetDefault,
}: {
  profile: ProviderProfile;
  isDefault: boolean;
  onEdit: (profile: ProviderProfile) => void;
  onDelete: (id: string) => void;
  onSetDefault: (profile: ProviderProfile) => void;
}) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <CardTitle className="text-sm font-medium">{profile.name}</CardTitle>
            {isDefault && (
              <Badge variant="default" className="text-xs">Default</Badge>
            )}
          </div>
          <Badge variant={profile.type === "local" ? "secondary" : "outline"}>
            {profile.type}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="text-xs text-muted-foreground">
          {profile.endpoint || "No endpoint configured"}
        </div>
        <div className="flex gap-2 flex-wrap">
          {profile.model_ids.map((model) => (
            <Badge key={model} variant="outline" className="text-xs">
              {model}
            </Badge>
          ))}
        </div>
        <div className="flex gap-2 pt-2">
          <Button size="sm" variant="outline" onClick={() => onEdit(profile)}>
            Edit
          </Button>
          {!isDefault && (
            <Button size="sm" variant="outline" onClick={() => onSetDefault(profile)}>
              Set Default
            </Button>
          )}
          <Button
            size="sm"
            variant="destructive"
            onClick={() => onDelete(profile.id)}
          >
            Delete
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

function ProviderForm({
  profile,
  onSubmit,
  onCancel,
}: {
  profile?: ProviderProfile;
  onSubmit: (data: ProviderProfileRequest) => void;
  onCancel: () => void;
}) {
  const [formData, setFormData] = useState({
    name: profile?.name || "",
    type: (profile?.type as "local" | "remote") || "remote",
    endpoint: profile?.endpoint || DEFAULT_ENDPOINTS.remote,
    model_ids: profile?.model_ids || [],
    // secret_ref in the form holds whatever the user typed this session.
    // It is NEVER seeded from the profile (the backend doesn't send the
    // stored value back), so on edit the input starts empty.
    secret_ref: "",
    enabled: profile?.enabled ?? true,
  });
  const [showSecret, setShowSecret] = useState(false);
  // Track whether the user has begun replacing an already-configured secret,
  // so we can render "Configured ✓ · Replace" instead of the input until
  // they opt in.
  const [replacingSecret, setReplacingSecret] = useState(
    !profile?.secret_configured
  );

  const handleTypeChange = (type: "local" | "remote") => {
    setFormData({
      ...formData,
      type,
      endpoint: DEFAULT_ENDPOINTS[type],
      model_ids: [],
    });
  };

  const addModel = (model: string) => {
    if (!model || formData.model_ids.includes(model)) return;
    setFormData({ ...formData, model_ids: [...formData.model_ids, model] });
  };

  const removeModel = (model: string) => {
    setFormData({
      ...formData,
      model_ids: formData.model_ids.filter((m) => m !== model),
    });
  };

  const applyPreset = (preset: ProviderPreset) => {
    setFormData({
      ...formData,
      name: preset.name,
      type: preset.type,
      endpoint: preset.endpoint,
      model_ids: [...preset.models],
    });
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    // Omit secret_ref from the payload when the user hasn't entered a new
    // value. The backend's update path treats an empty secret_ref as "keep
    // the existing reference," so leaving it out is the safe path.
    const payload: ProviderProfileRequest = {
      name: formData.name,
      type: formData.type,
      endpoint: formData.endpoint,
      model_ids: formData.model_ids,
      enabled: formData.enabled,
    };
    if (formData.secret_ref.trim()) {
      payload.secret_ref = formData.secret_ref.trim();
    }
    onSubmit(payload);
  };

  const allModels = MODEL_CATALOG[formData.type] || [];
  const selected = new Set(formData.model_ids);
  const unselectedGroups = allModels
    .map((g) => ({ ...g, models: g.models.filter((m) => !selected.has(m)) }))
    .filter((g) => g.models.length > 0);

  const presets = PROVIDER_PRESETS.filter((p) => p.type === formData.type);

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      {/* Presets */}
      {!profile && (
        <div className="space-y-2">
          <label className="text-sm font-medium">Quick Setup</label>
          <div className="flex flex-wrap gap-2">
            {presets.map((preset) => (
              <button
                key={preset.name}
                type="button"
                onClick={() => applyPreset(preset)}
                className="px-3 py-1.5 text-xs rounded-md border border-input bg-background hover:bg-muted transition-colors"
              >
                {preset.name}
              </button>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            Click a preset to auto-fill provider details
          </p>
        </div>
      )}

      <div className="space-y-2">
        <label className="text-sm font-medium">Name</label>
        <Input
          value={formData.name}
          onChange={(e) => setFormData({ ...formData, name: e.target.value })}
          required
        />
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium">Type</label>
        <select
          className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
          value={formData.type}
          onChange={(e) => handleTypeChange(e.target.value as "local" | "remote")}
        >
          <option value="remote">Remote (Cloud)</option>
          <option value="local">Local (Self-hosted)</option>
        </select>
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium">Endpoint URL</label>
        <Input
          value={formData.endpoint}
          onChange={(e) => setFormData({ ...formData, endpoint: e.target.value })}
          placeholder="https://api.openai.com/v1"
        />
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium">Models</label>
        <div className="flex gap-2">
          <select
            className="flex-1 h-9 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
            value=""
            onChange={(e) => {
              addModel(e.target.value);
              e.target.value = "";
            }}
          >
            <option value="">Select a model to add...</option>
            {unselectedGroups.map((group) => (
              <optgroup key={group.category} label={group.category}>
                {group.models.map((model) => (
                  <option key={model} value={model}>
                    {model}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </div>
        {formData.model_ids.length > 0 && (
          <div className="flex gap-2 flex-wrap mt-2">
            {formData.model_ids.map((model) => (
              <Badge key={model} variant="secondary" className="text-xs gap-1">
                {model}
                <button
                  type="button"
                  onClick={() => removeModel(model)}
                  className="hover:text-destructive"
                >
                  <X className="w-3 h-3" />
                </button>
              </Badge>
            ))}
          </div>
        )}
        {formData.model_ids.length === 0 && (
          <p className="text-xs text-muted-foreground">No models selected yet.</p>
        )}
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium">API Key</label>
        {!replacingSecret ? (
          <div className="flex items-center gap-3 text-sm">
            <Badge variant="secondary" className="gap-1">
              Configured ✓
            </Badge>
            <button
              type="button"
              onClick={() => setReplacingSecret(true)}
              className="text-xs text-primary hover:underline"
            >
              Replace
            </button>
            <p className="text-xs text-muted-foreground">
              Leave as-is to keep the existing key.
            </p>
          </div>
        ) : (
          <>
            <div className="relative">
              <Input
                type={showSecret ? "text" : "password"}
                value={formData.secret_ref}
                onChange={(e) => setFormData({ ...formData, secret_ref: e.target.value })}
                placeholder={profile?.secret_configured ? "Enter new API key to replace" : "API key"}
                className="pr-10"
                autoComplete="off"
              />
              <button
                type="button"
                onClick={() => setShowSecret((s) => !s)}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                tabIndex={-1}
                aria-label={showSecret ? "Hide API key" : "Show API key"}
              >
                {showSecret ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            </div>
            <p className="text-xs text-muted-foreground">
              Stored in the OS keyring. The raw key never touches the database.
            </p>
            {profile?.secret_configured && (
              <button
                type="button"
                onClick={() => {
                  setReplacingSecret(false);
                  setFormData({ ...formData, secret_ref: "" });
                }}
                className="text-xs text-muted-foreground hover:underline"
              >
                Cancel replacement
              </button>
            )}
          </>
        )}
      </div>
      <div className="flex items-center gap-2">
        <input
          type="checkbox"
          id="enabled"
          checked={formData.enabled}
          onChange={(e) => setFormData({ ...formData, enabled: e.target.checked })}
          className="rounded border-gray-300"
        />
        <label htmlFor="enabled" className="text-sm">Enabled</label>
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit">{profile ? "Update" : "Create"}</Button>
      </DialogFooter>
    </form>
  );
}

export function ProviderSettings() {
  const [profiles, setProfiles] = useState<ProviderProfile[]>([]);
  const [defaultSettings, setDefaultSettings] = useState<LLMDefaultSettings>({ provider_id: "", model_id: "" });
  const [loading, setLoading] = useState(true);
  const [editingProfile, setEditingProfile] = useState<ProviderProfile | null>(null);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [deleteTargetId, setDeleteTargetId] = useState<string | null>(null);

  useEffect(() => {
    loadData();
  }, []);

  const loadData = async () => {
    try {
      const [profilesData, settingsData] = await Promise.all([
        providersApi.list(),
        settingsApi.getLLMDefault(),
      ]);
      setProfiles(profilesData.profiles || []);
      setDefaultSettings(settingsData);
    } catch (error) {
      console.error("Failed to load provider data:", error);
    } finally {
      setLoading(false);
    }
  };

  const handleCreate = async (data: ProviderProfileRequest) => {
    try {
      await providersApi.create(data);
      setDialogOpen(false);
      loadData();
    } catch (error) {
      console.error("Failed to create provider:", error);
    }
  };

  const handleUpdate = async (data: ProviderProfileRequest) => {
    if (!editingProfile) return;
    try {
      await providersApi.update(editingProfile.id, data);
      setEditingProfile(null);
      setDialogOpen(false);
      loadData();
    } catch (error) {
      console.error("Failed to update provider:", error);
    }
  };

  const requestDelete = (id: string) => {
    setDeleteTargetId(id);
  };

  const confirmDelete = async () => {
    if (!deleteTargetId) return;
    try {
      await providersApi.delete(deleteTargetId);
      loadData();
    } catch (error) {
      console.error("Failed to delete provider:", error);
    }
  };

  const handleSetDefault = async (profile: ProviderProfile) => {
    const modelId = profile.model_ids[0] || "";
    try {
      await settingsApi.setLLMDefault({ provider_id: profile.id, model_id: modelId });
      loadData();
    } catch (error) {
      console.error("Failed to set default:", error);
    }
  };

  if (loading) {
    return <div className="p-4">Loading providers...</div>;
  }

  return (
    <div className="p-4 space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">LLM Providers</h2>
          <p className="text-sm text-muted-foreground">
            Configure AI model providers. Select a default for normal use, or enable advanced mode per-assistant.
          </p>
        </div>
        <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
          <DialogTrigger asChild>
            <Button
              onClick={() => {
                setEditingProfile(null);
                setDialogOpen(true);
              }}
            >
              Add Provider
            </Button>
          </DialogTrigger>
          <DialogContent className="max-w-lg max-h-[90vh] overflow-y-auto">
            <DialogHeader>
              <DialogTitle>
                {editingProfile ? "Edit Provider" : "Add Provider"}
              </DialogTitle>
            </DialogHeader>
            <ProviderForm
              profile={editingProfile || undefined}
              onSubmit={editingProfile ? handleUpdate : handleCreate}
              onCancel={() => {
                setEditingProfile(null);
                setDialogOpen(false);
              }}
            />
          </DialogContent>
        </Dialog>
      </div>

      {defaultSettings.provider_id && (
        <div className="bg-muted rounded-lg p-3 text-sm">
          <span className="font-medium">Default:</span>{" "}
          {profiles.find((p) => p.id === defaultSettings.provider_id)?.name || defaultSettings.provider_id}
          {" / "}
          {defaultSettings.model_id}
        </div>
      )}

      {profiles.length === 0 ? (
        <div className="text-muted-foreground">No providers configured yet</div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {profiles.map((profile) => (
            <ProviderCard
              key={profile.id}
              profile={profile}
              isDefault={profile.id === defaultSettings.provider_id}
              onEdit={(p) => {
                setEditingProfile(p);
                setDialogOpen(true);
              }}
              onDelete={requestDelete}
              onSetDefault={handleSetDefault}
            />
          ))}
        </div>
      )}

      <ConfirmDialog
        open={deleteTargetId !== null}
        onOpenChange={(next) => !next && setDeleteTargetId(null)}
        title="Delete provider profile?"
        description="This removes the provider and its stored API key. Assistants that use it will fall back to the global default."
        confirmLabel="Delete"
        destructive
        onConfirm={confirmDelete}
      />
    </div>
  );
}
