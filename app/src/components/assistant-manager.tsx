import { useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
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
import { ApiError, assistantsApi, toolsApi, connectorsApi, providersApi, settingsApi } from "@/lib/api";
import { AssistantBindingsPanel } from "@/components/assistant-bindings-panel";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import type {
  Assistant,
  CreateAssistantRequest,
  ConnectorConfig,
  ProviderProfile,
  PermissionRule,
  ChannelConfig,
  SafetyProfile,
} from "@/types/api";
import { FolderPreview } from "@/components/folder-preview";
import type { FileNode } from "@/components/folder-preview";
import { ToggleSwitch } from "@/components/ui/toggle-switch";
import { labels } from "@/lib/labels";
import { Plus, Trash2, Download } from "lucide-react";
import { Select, SelectItem } from "@/components/ui/select";
import { RemoteTemplateBrowser } from "@/components/remote-template-browser";

// Capability ceiling labels — only items that map to a real permission
// engine capability (filesystem.*, command.exec, network.outgoing). Memory
// and Connector were here historically but they're features with their
// own config sections below (Memory toggle, channel bindings via the
// Plugins tab) and listing them here just duplicated the controls. Code
// dropped because it overlapped with filesystem.read.
const PREDEFINED_CAPABILITIES = [
  { id: "filesystem", label: "Filesystem", description: "Read and write files" },
  { id: "command", label: "Command", description: "Execute shell commands" },
  { id: "web", label: "Web", description: "Fetch web pages and search" },
];

// Maps a concrete capability string to its family name. Used for ceiling
// validation: a policy rule for "filesystem.write" requires the
// "filesystem" family to be ticked in Declared capabilities.
const CAPABILITY_FAMILIES: Record<string, string> = {
  "filesystem.read": "filesystem",
  "filesystem.write": "filesystem",
  "filesystem.context": "filesystem",
  "command.exec": "command",
  "network.outgoing": "web",
};

// Derive the family for a capability, handling wildcards like "filesystem.*".
function familyForCapability(cap: string): string | null {
  if (cap.endsWith(".*")) {
    return cap.slice(0, -2);
  }
  return CAPABILITY_FAMILIES[cap] ?? null;
}

// Returns true if the assistant's declared capabilities permit this capability.
function isCapabilityAllowed(cap: string, declared: string[]): boolean {
  if (declared.length === 0) return true;
  const family = familyForCapability(cap);
  if (!family) return true; // plugin-scoped or unknown — not ceiling-gated
  return declared.includes(family) || declared.includes(cap);
}

interface RuleCeilingViolation {
  ruleIndex: number;
  capability: string;
  family: string;
}

const DEFAULT_PERMISSION_RULES: PermissionRule[] = [
  { capability: "llm.chat", mode: "confirm" },
  { capability: "filesystem.read", mode: "confirm" },
  { capability: "filesystem.write", mode: "confirm" },
  { capability: "command.exec", mode: "confirm" },
  { capability: "network.outgoing", mode: "confirm" },
];

function rulesForSafetyProfile(profile: SafetyProfile): PermissionRule[] {
  if (profile === "fast") {
    return [
      { capability: "llm.chat", mode: "allow" },
      { capability: "filesystem.read", mode: "allow" },
      { capability: "filesystem.write", mode: "allow" },
      { capability: "command.exec", mode: "allow" },
      { capability: "network.outgoing", mode: "confirm" },
    ];
  }
  if (profile === "balanced") {
    return [
      { capability: "llm.chat", mode: "allow" },
      { capability: "filesystem.read", mode: "allow" },
      { capability: "filesystem.write", mode: "confirm" },
      { capability: "command.exec", mode: "confirm" },
      { capability: "network.outgoing", mode: "confirm" },
    ];
  }
  return [...DEFAULT_PERMISSION_RULES];
}

function AssistantCard({
  assistant,
  onEdit,
  onDelete,
}: {
  assistant: Assistant;
  onEdit: (assistant: Assistant) => void;
  onDelete: (id: string) => void;
}) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <CardTitle className="text-sm font-medium">{assistant.name}</CardTitle>
          <Badge variant="outline">{assistant.role}</Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="text-sm text-muted-foreground line-clamp-2">
          {assistant.system_prompt}
        </div>
        <div className="flex gap-2 flex-wrap">
          {assistant.capabilities?.map((cap) => (
            <Badge key={cap} variant="secondary" className="text-xs">
              {cap}
            </Badge>
          ))}
          {assistant.memory_policy?.enabled && (
            <Badge variant="outline" className="text-xs border-amber-500 text-amber-600">
              memory:{assistant.memory_policy.scope}
            </Badge>
          )}
          {assistant.contexts && assistant.contexts.length > 0 && (
            <Badge variant="outline" className="text-xs border-blue-500 text-blue-600">
              {assistant.contexts.length} context{assistant.contexts.length > 1 ? "s" : ""}
            </Badge>
          )}
          {assistant.channel_configs && assistant.channel_configs.length > 0 && (
            <Badge variant="outline" className="text-xs border-green-500 text-green-600">
              {assistant.channel_configs.reduce((acc, cc) => acc + cc.connections.length, 0)}{" "}
              connection
              {assistant.channel_configs.reduce((acc, cc) => acc + cc.connections.length, 0) > 1 ? "s" : ""}
            </Badge>
          )}
        </div>
        <div className="flex gap-2 pt-2">
          <Button size="sm" variant="outline" onClick={() => onEdit(assistant)}>
            Edit
          </Button>
          <Button size="sm" variant="destructive" onClick={() => onDelete(assistant.id)}>
            Delete
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

interface CeilingViolation {
  capability: string;
  family: string;
}

interface CeilingViolationError {
  code: "ceiling_violation";
  message: string;
  violations: CeilingViolation[];
  suggested_capabilities: string[];
}

function AssistantForm({
  assistant,
  onSubmit,
  onCancel,
  submitError,
}: {
  assistant?: Assistant;
  onSubmit: (data: CreateAssistantRequest) => void;
  onCancel: () => void;
  submitError?: string | CeilingViolationError | null;
}) {
  const [formData, setFormData] = useState<CreateAssistantRequest>({
    name: assistant?.name || "",
    template_id: assistant?.template_id || "",
    tagline: assistant?.tagline || "",
    role: assistant?.role || "",
    best_for: assistant?.best_for || "",
    not_for: assistant?.not_for || "",
    suggested_model: assistant?.suggested_model || "",
    system_prompt: assistant?.system_prompt || "",
    channels: assistant?.channels || [],
    channel_configs: assistant?.channel_configs || [],
    capabilities: assistant?.capabilities || [],
    contexts: assistant?.contexts || [],
    memory_policy: assistant?.memory_policy || { enabled: true, scope: "workspace" },
    permission_policy: assistant?.permission_policy || { rules: [] },
    model_policy: assistant?.model_policy || {
      mode: "global_default",
      preferred: "",
      allow_fallback: true,
    },
  });

  const [availableConnectors, setAvailableConnectors] = useState<ConnectorConfig[]>([]);
  const [availableProviders, setAvailableProviders] = useState<ProviderProfile[]>([]);
  const [templates, setTemplates] = useState<Assistant[]>([]);
  const [previewData, setPreviewData] = useState<
    Record<number, { tree?: FileNode; stats?: { file_count: number; dir_count: number; total_size: number }; loading: boolean }>
  >({});
  const [showAdvancedModel, setShowAdvancedModel] = useState(false);
	const [applyingProfile, setApplyingProfile] = useState(false);
	const [showRemoteTemplates, setShowRemoteTemplates] = useState(false);

	useEffect(() => {
    connectorsApi
      .listConfigs()
      .then((data) => setAvailableConnectors(data.connectors.filter((c) => c.enabled)))
      .catch((err) => console.error("Failed to load connectors:", err));
    if (!assistant) {
      settingsApi
        .getSafetyProfile()
        .then((data) => {
          setFormData((prev) => {
            if ((prev.permission_policy?.rules?.length || 0) > 0) {
              return prev;
            }
            return {
              ...prev,
              permission_policy: { rules: rulesForSafetyProfile(data.profile) },
            };
          });
        })
        .catch((err) => {
          console.error("Failed to load safety profile:", err);
          setFormData((prev) => {
            if ((prev.permission_policy?.rules?.length || 0) > 0) {
              return prev;
            }
            return {
              ...prev,
              permission_policy: { rules: [...DEFAULT_PERMISSION_RULES] },
            };
          });
        });
    }
    assistantsApi
      .listTemplates()
      .then((data) => setTemplates(data.templates))
      .catch((err) => console.error("Failed to load assistant templates:", err));
    providersApi
      .list()
      .then((data) => setAvailableProviders(data.profiles.filter((p) => p.enabled)))
      .catch((err) => console.error("Failed to load providers:", err));
  }, [assistant]);

  const applyTemplate = (tpl: Assistant) => {
    setFormData((prev) => ({
      ...prev,
      template_id: tpl.template_id || "",
      name: tpl.name,
      tagline: tpl.tagline || "",
      role: tpl.role,
      best_for: tpl.best_for || "",
      not_for: tpl.not_for || "",
      suggested_model: tpl.suggested_model || "",
      system_prompt: tpl.system_prompt,
      channels: tpl.channels || [],
      channel_configs: tpl.channel_configs || [],
      capabilities: tpl.capabilities || [],
      contexts: tpl.contexts || [],
      memory_policy: tpl.memory_policy || { enabled: true, scope: "workspace" },
      permission_policy: tpl.permission_policy || { rules: [...DEFAULT_PERMISSION_RULES] },
      model_policy: tpl.model_policy || prev.model_policy,
    }));
  };

  const loadPreview = async (index: number, path: string) => {
    if (!path) return;
    setPreviewData((prev) => ({ ...prev, [index]: { ...prev[index], loading: true } }));
    try {
      const data = await toolsApi.previewFolderContext(path);
      setPreviewData((prev) => ({
        ...prev,
        [index]: {
          tree: data.tree as FileNode,
          stats: data.stats as { file_count: number; dir_count: number; total_size: number },
          loading: false,
        },
      }));
    } catch (err) {
      console.error("Failed to load preview:", err);
      setPreviewData((prev) => ({ ...prev, [index]: { loading: false } }));
    }
  };

  const capabilitiesSectionRef = useRef<HTMLDivElement | null>(null);
  const [capabilitiesHighlighted, setCapabilitiesHighlighted] = useState(false);

  // Real-time ceiling validation: compute which policy rules reference a
  // capability family that is not ticked in Declared capabilities.
  const ruleCeilingViolations = (() => {
    const declared = formData.capabilities || [];
    const violations: RuleCeilingViolation[] = [];
    const rules = formData.permission_policy?.rules || [];
    for (let i = 0; i < rules.length; i++) {
      const rule = rules[i];
      if (rule.mode === "deny") continue;
      if (isCapabilityAllowed(rule.capability, declared)) continue;
      const family = familyForCapability(rule.capability);
      if (family) {
        violations.push({ ruleIndex: i, capability: rule.capability, family });
      }
    }
    return violations;
  })();

  const violatedFamilies = new Set(ruleCeilingViolations.map((v) => v.family));

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (ruleCeilingViolations.length > 0) {
      // Block submission and scroll to the capabilities section so the
      // inline errors are visible.
      capabilitiesSectionRef.current?.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
      setCapabilitiesHighlighted(true);
      window.setTimeout(() => setCapabilitiesHighlighted(false), 1800);
      return;
    }
    onSubmit(formData);
  };

  // Scroll the user up to the Declared capabilities section and briefly
  // highlight it after applying the suggested fix from a ceiling-violation
  // error. Without the scroll the user clicks "Add missing families" and
  // sees nothing change because their viewport is at the submit button.
  const applySuggestedCapabilities = (caps: string[]) => {
    setFormData({ ...formData, capabilities: caps });
    requestAnimationFrame(() => {
      capabilitiesSectionRef.current?.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
      setCapabilitiesHighlighted(true);
      window.setTimeout(() => setCapabilitiesHighlighted(false), 1800);
    });
  };

  const getConnectorConnections = (connector: ConnectorConfig): { id: string; name: string }[] => {
    if (connector.name === "telegram") {
      const connections = (connector.config?.connections as { id: string; name: string; enabled: boolean }[]) || [];
      return connections.filter((c) => c.enabled);
    }
    return [];
  };

  const toggleConnection = (connectorName: string, connectionId: string) => {
    const current = formData.channel_configs || [];
    const existing = current.find((cc) => cc.connector === connectorName);

    let updated: ChannelConfig[];
    if (existing) {
      const hasConnection = existing.connections.includes(connectionId);
      const newConnections = hasConnection
        ? existing.connections.filter((id) => id !== connectionId)
        : [...existing.connections, connectionId];

      if (newConnections.length === 0) {
        updated = current.filter((cc) => cc.connector !== connectorName);
      } else {
        updated = current.map((cc) =>
          cc.connector === connectorName ? { ...cc, connections: newConnections } : cc
        );
      }
    } else {
      updated = [...current, { connector: connectorName, connections: [connectionId] }];
    }

    setFormData({ ...formData, channel_configs: updated });
  };

  const isConnectionSelected = (connectorName: string, connectionId: string) => {
    return formData.channel_configs?.some(
      (cc) => cc.connector === connectorName && cc.connections.includes(connectionId)
    );
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-5">
      {!assistant && templates.length > 0 && (
        <div className="border rounded-lg p-3 space-y-3">
          <label className="text-sm font-medium">Start from a template</label>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            {templates.map((tpl) => {
              const selected = formData.template_id === (tpl.template_id || "");
              return (
                <button
                  key={tpl.id}
                  type="button"
                  onClick={() => applyTemplate(tpl)}
                  className={`text-left rounded-md border p-3 transition-colors hover:bg-muted/40 ${
                    selected ? "border-primary bg-primary/5" : "border-border"
                  }`}
                >
                  <div className="flex items-center justify-between gap-2">
                    <p className="text-sm font-medium">{tpl.name}</p>
                    {tpl.suggested_model && (
                      <Badge variant="outline" className="text-[10px]">
                        {tpl.suggested_model}
                      </Badge>
                    )}
                  </div>
                  {tpl.tagline && <p className="text-xs text-muted-foreground mt-1">{tpl.tagline}</p>}
                  <p className="text-xs mt-2">
                    <span className="font-medium">Best for:</span> {tpl.best_for || "-"}
                  </p>
                  <p className="text-xs text-muted-foreground">
                    <span className="font-medium text-foreground">Not for:</span> {tpl.not_for || "-"}
                  </p>
                </button>
              );
            })}
          </div>
        </div>
      )}

      {/* Basic Info */}
      <div className="space-y-2">
        <label className="text-sm font-medium">Name</label>
        <Input
          value={formData.name}
          onChange={(e) => setFormData({ ...formData, name: e.target.value })}
          placeholder="e.g. Code Reviewer"
          required
        />
      </div>

      <div className="space-y-2">
        <label className="text-sm font-medium">Role</label>
        <p className="text-xs text-muted-foreground">
          Defines how the assistant behaves — e.g. Developer, Product Manager, Designer, Researcher
        </p>
        <Input
          value={formData.role}
          onChange={(e) => setFormData({ ...formData, role: e.target.value })}
          placeholder="e.g. Senior Developer"
          required
        />
      </div>

      <div className="space-y-2">
        <label className="text-sm font-medium">System Prompt</label>
        <p className="text-xs text-muted-foreground">
          Instructions that shape the assistant&apos;s personality, expertise, and how it responds
        </p>
        <textarea
          className="flex min-h-[100px] w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
          value={formData.system_prompt}
          onChange={(e) => setFormData({ ...formData, system_prompt: e.target.value })}
          placeholder="You are a helpful coding assistant..."
          required
        />
      </div>

      {/* Capabilities — declared intent, not enforcement. */}
      <div
        ref={capabilitiesSectionRef}
        className={
          "border rounded-lg p-3 space-y-3 transition-colors duration-700 " +
          (capabilitiesHighlighted
            ? "ring-2 ring-amber-400 bg-amber-50/40 dark:bg-amber-950/20"
            : "")
        }
      >
        <label className="text-sm font-medium">Declared capabilities</label>
        <p className="text-xs text-muted-foreground">
          What this assistant says it needs. Used for documentation and to generate sensible
          permission rules below — actual gating happens in <strong>Permissions</strong> further
          down. Use the Permissions section to set allow / confirm / deny per capability.
        </p>
        {violatedFamilies.size > 0 && (
          <p className="text-xs text-amber-600 dark:text-amber-400 font-medium">
            {Array.from(violatedFamilies).map((f) => (
              <span key={f}>
                Tick <strong className="capitalize">{f}</strong> to activate the matching permission rule
                {violatedFamilies.size > 1 ? "; " : ""}
              </span>
            ))}
          </p>
        )}
        <div className="grid grid-cols-2 gap-2">
          {PREDEFINED_CAPABILITIES.map((cap) => {
            const isViolated = violatedFamilies.has(cap.id);
            return (
              <label
                key={cap.id}
                className={
                  "flex items-start gap-2 cursor-pointer p-2 rounded hover:bg-muted/50 " +
                  (isViolated ? "bg-amber-50 dark:bg-amber-950/30 ring-1 ring-amber-300 dark:ring-amber-700" : "")
                }
              >
                <input
                  type="checkbox"
                  className="mt-0.5 rounded border-gray-300 h-4 w-4 text-primary focus:ring-ring"
                  checked={formData.capabilities?.includes(cap.id) || false}
                  onChange={(e) => {
                    const current = formData.capabilities || [];
                    const updated = e.target.checked
                      ? [...current, cap.id]
                      : current.filter((c) => c !== cap.id);
                    setFormData({ ...formData, capabilities: updated });
                  }}
                />
                <div className="flex flex-col">
                  <span className="text-sm font-medium">{cap.label}</span>
                  <span className="text-xs text-muted-foreground">{cap.description}</span>
                </div>
              </label>
            );
          })}
        </div>
      </div>

      {/* Channel Connections */}
      {availableConnectors.length > 0 && (
        <div className="border rounded-lg p-3 space-y-3">
          <label className="text-sm font-medium">Channel Connections</label>
          <p className="text-xs text-muted-foreground">
            Select which connections this assistant can use to receive and send messages
          </p>
          <div className="space-y-3">
            {availableConnectors.map((connector) => {
              const connections = getConnectorConnections(connector);
              if (connections.length === 0) return null;
              return (
                <div key={connector.name} className="space-y-2">
                  <h4 className="text-sm font-medium text-muted-foreground">{connector.manifest.name}</h4>
                  <div className="space-y-1.5 pl-2">
                    {connections.map((conn) => (
                      <label key={conn.id} className="flex items-center gap-2 cursor-pointer">
                        <input
                          type="checkbox"
                          className="rounded border-gray-300 h-4 w-4 text-primary focus:ring-ring"
                          checked={isConnectionSelected(connector.name, conn.id)}
                          onChange={() => toggleConnection(connector.name, conn.id)}
                        />
                        <span className="text-sm">{conn.name}</span>
                      </label>
                    ))}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}

      {/* Model Selection */}
      <div className="border rounded-lg p-3 space-y-3">
        <div className="flex items-center justify-between">
          <label className="text-sm font-medium">Model</label>
          <button
            type="button"
            onClick={() => setShowAdvancedModel(!showAdvancedModel)}
            className="text-xs text-muted-foreground hover:text-foreground underline"
          >
            {showAdvancedModel ? "Simple" : "Advanced"}
          </button>
        </div>
        <p className="text-xs text-muted-foreground">
          {formData.model_policy?.mode === "global_default"
            ? "Uses the global default model"
            : "Uses a model specific to this assistant"}
        </p>

        <div className="space-y-2">
          <label className="text-sm">Model Selection</label>
          <Select
            value={formData.model_policy?.mode || "global_default"}
            onValueChange={(value: string) => {
              const mode = value as "global_default" | "assistant_override";
              setFormData({
                ...formData,
                model_policy: {
                  mode,
                  preferred: mode === "assistant_override" ? formData.model_policy?.preferred || "" : undefined,
                  fallback: mode === "global_default" ? undefined : formData.model_policy?.fallback,
                  local_only: mode === "global_default" ? undefined : formData.model_policy?.local_only,
                  allow_fallback: mode === "global_default" ? undefined : formData.model_policy?.allow_fallback,
                },
              });
            }}
            placeholder="Select mode"
          >
            <SelectItem value="global_default">Use global default</SelectItem>
            <SelectItem value="assistant_override">Use specific model</SelectItem>
          </Select>
        </div>

        {formData.model_policy?.mode === "assistant_override" && (
          <div className="space-y-3">
            <div className="space-y-2">
              <label className="text-sm">Preferred Model</label>
              <select
                className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
                value={formData.model_policy?.preferred || ""}
                onChange={(e) =>
                  setFormData({
                    ...formData,
                    model_policy: {
                      mode: "assistant_override",
                      preferred: e.target.value,
                      fallback: formData.model_policy?.fallback,
                      local_only: formData.model_policy?.local_only,
                      allow_fallback: formData.model_policy?.allow_fallback,
                    },
                  })
                }
              >
                <option value="">Select a model...</option>
                {availableProviders.map((provider) =>
                  provider.model_ids.map((model) => (
                    <option key={`${provider.id}:${model}`} value={`${provider.id}:${model}`}>
                      {provider.name} — {model}
                    </option>
                  ))
                )}
              </select>
            </div>

            {showAdvancedModel && (
              <>
                <div className="space-y-2">
                  <label className="text-sm">Fallback Model</label>
                  <select
                    className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
                    value={formData.model_policy?.fallback || ""}
                    onChange={(e) =>
                      setFormData({
                        ...formData,
                        model_policy: {
                          mode: "assistant_override",
                          preferred: formData.model_policy?.preferred || "",
                          fallback: e.target.value || undefined,
                          local_only: formData.model_policy?.local_only,
                          allow_fallback: formData.model_policy?.allow_fallback,
                        },
                      })
                    }
                  >
                    <option value="">None</option>
                    {availableProviders.map((provider) =>
                      provider.model_ids.map((model) => (
                        <option key={`${provider.id}:${model}`} value={`${provider.id}:${model}`}>
                          {provider.name} — {model}
                        </option>
                      ))
                    )}
                  </select>
                </div>
                <label className="flex items-center gap-2 cursor-pointer">
                  <input
                    type="checkbox"
                    className="rounded border-gray-300 h-4 w-4 text-primary focus:ring-ring"
                    checked={formData.model_policy?.local_only || false}
                    onChange={(e) =>
                      setFormData({
                        ...formData,
                        model_policy: {
                          mode: "assistant_override",
                          preferred: formData.model_policy?.preferred || "",
                          fallback: formData.model_policy?.fallback,
                          local_only: e.target.checked,
                          allow_fallback: formData.model_policy?.allow_fallback,
                        },
                      })
                    }
                  />
                  <span className="text-sm">Local-only providers</span>
                </label>
              </>
            )}
          </div>
        )}
      </div>

      {/* Memory Policy */}
      <div className="border rounded-lg p-3 space-y-3">
        <div className="flex items-center justify-between">
          <label className="text-sm font-medium">Memory</label>
          <ToggleSwitch
            checked={formData.memory_policy?.enabled || false}
            onChange={(checked) =>
              setFormData({
                ...formData,
                memory_policy: {
                  enabled: checked,
                  scope: formData.memory_policy?.scope || "workspace",
                },
              })
            }
          />
        </div>
        {formData.memory_policy?.enabled && (
          <div className="space-y-2">
            <label className="text-sm font-medium">Scope</label>
            <select
              className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm"
              value={formData.memory_policy?.scope || "workspace"}
              onChange={(e) =>
                setFormData({
                  ...formData,
                  memory_policy: {
                    enabled: true,
                    scope: e.target.value,
                  },
                })
              }
            >
              <option value="workspace">Workspace</option>
              <option value="profile">Profile</option>
            </select>
            <p className="text-xs text-muted-foreground">
              {formData.memory_policy?.scope === "profile"
                ? "Profile memories persist across all workspaces."
                : "Workspace memories are local to this project."}
            </p>
          </div>
        )}
      </div>

      {/* Permissions */}
      <div className="border rounded-lg p-3 space-y-3">
        <div className="flex items-center justify-between">
          <label className="text-sm font-medium">Permissions</label>
          <div className="flex items-center gap-2">
            {assistant?.id && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={applyingProfile}
                onClick={async () => {
                  setApplyingProfile(true);
                  try {
                    const data = await assistantsApi.applySafetyProfile(assistant.id);
                    setFormData((prev) => ({
                      ...prev,
                      permission_policy: data.assistant.permission_policy,
                    }));
                  } catch (err) {
                    console.error("Failed to apply safety profile:", err);
                  } finally {
                    setApplyingProfile(false);
                  }
                }}
              >
                {applyingProfile ? "Applying..." : "Apply Safety Profile"}
              </Button>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() =>
                setFormData({
                  ...formData,
                  permission_policy: {
                    rules: [...(formData.permission_policy?.rules || []), { capability: "", mode: "confirm" }],
                  },
                })
              }
            >
              <Plus className="w-3 h-3 mr-1" />
              Add Rule
            </Button>
          </div>
        </div>
        <p className="text-xs text-muted-foreground">
          Control what actions require approval, are allowed, or denied
        </p>
        <div className="space-y-2">
          {(formData.permission_policy?.rules || []).length === 0 && (
            <div className="text-sm text-muted-foreground">No custom rules. Using default permissions.</div>
          )}
          {(formData.permission_policy?.rules || []).map((rule, i) => {
            const violation = ruleCeilingViolations.find((v) => v.ruleIndex === i);
            return (
              <div key={i} className="space-y-1">
                <div className="flex items-center gap-2">
                  <Input
                    value={rule.capability}
                    onChange={(e) => {
                      const newRules = [...(formData.permission_policy?.rules || [])];
                      newRules[i] = { ...rule, capability: e.target.value };
                      setFormData({ ...formData, permission_policy: { rules: newRules } });
                    }}
                    placeholder="e.g. filesystem.write"
                    className={
                      "flex-1 text-sm " +
                      (violation ? "border-amber-400 focus-visible:ring-amber-400" : "")
                    }
                  />
                  <select
                    value={rule.mode}
                    onChange={(e) => {
                      const newRules = [...(formData.permission_policy?.rules || [])];
                      newRules[i] = { ...rule, mode: e.target.value as PermissionRule["mode"] };
                      setFormData({ ...formData, permission_policy: { rules: newRules } });
                    }}
                    className="h-9 rounded-md border border-input bg-transparent px-2 py-1 text-sm shadow-sm"
                  >
                    <option value="allow">Allow</option>
                    <option value="confirm">Confirm</option>
                    <option value="deny">Deny</option>
                  </select>
                  <button
                    type="button"
                    onClick={() => {
                      const newRules = [...(formData.permission_policy?.rules || [])];
                      newRules.splice(i, 1);
                      setFormData({ ...formData, permission_policy: { rules: newRules } });
                    }}
                    className="p-1.5 rounded hover:bg-destructive/10 text-muted-foreground hover:text-destructive"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>
                {violation && (
                  <p className="text-xs text-amber-600 dark:text-amber-400">
                    Tick the <strong className="capitalize">{violation.family}</strong> box in
                    Declared capabilities to allow this rule to take effect.
                  </p>
                )}
                {rule.capability === "command.exec" && rule.mode === "allow" && (
                  <div className="ml-0">
                    <label className="text-xs text-muted-foreground">
                      Allowed binaries (comma-separated; empty = any binary the shlex parser permits)
                    </label>
                    <Input
                      value={
                        Array.isArray(rule.constraints?.allowed_binaries)
                          ? (rule.constraints!.allowed_binaries as string[]).join(", ")
                          : ""
                      }
                      onChange={(e) => {
                        const value = e.target.value;
                        const parsed = value
                          .split(",")
                          .map((s) => s.trim())
                          .filter(Boolean);
                        const newRules = [...(formData.permission_policy?.rules || [])];
                        const nextConstraints = { ...(rule.constraints || {}) };
                        if (parsed.length === 0) {
                          delete nextConstraints.allowed_binaries;
                        } else {
                          nextConstraints.allowed_binaries = parsed;
                        }
                        newRules[i] = {
                          ...rule,
                          constraints: Object.keys(nextConstraints).length
                            ? nextConstraints
                            : undefined,
                        };
                        setFormData({
                          ...formData,
                          permission_policy: { rules: newRules },
                        });
                      }}
                      placeholder="git, go, npm"
                      className="mt-1 text-sm"
                    />
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>

      {/* Folder Contexts */}
      <div className="border rounded-lg p-3 space-y-3">
        <label className="text-sm font-medium">Folder Contexts</label>
        <div className="space-y-3">
          {formData.contexts?.map((ctx, i) => (
            <div key={i} className="space-y-2">
              <div className="flex items-center gap-2">
                <Input
                  value={ctx.path}
                  onChange={(e) => {
                    const newContexts = [...(formData.contexts || [])];
                    newContexts[i] = { ...ctx, path: e.target.value };
                    setFormData({ ...formData, contexts: newContexts });
                  }}
                  placeholder="/path/to/folder"
                  className="flex-1"
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={async () => {
                    try {
                      const selected = await invoke<string | null>("pick_workspace_folder", {
                        initialPath: ctx.path || ".",
                      });
                      if (selected) {
                        const newContexts = [...(formData.contexts || [])];
                        newContexts[i] = { ...ctx, path: selected };
                        setFormData({ ...formData, contexts: newContexts });
                      }
                    } catch {
                      // ignore
                    }
                  }}
                >
                  Pick
                </Button>
                <Button type="button" variant="outline" size="sm" onClick={() => loadPreview(i, ctx.path)} disabled={!ctx.path}>
                  Preview
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    const newContexts = [...(formData.contexts || [])];
                    newContexts.splice(i, 1);
                    setFormData({ ...formData, contexts: newContexts });
                    setPreviewData((prev) => {
                      const next = { ...prev };
                      delete next[i];
                      return next;
                    });
                  }}
                >
                  Remove
                </Button>
              </div>
              {previewData[i] && (
                <FolderPreview
                  path={ctx.path}
                  tree={previewData[i].tree}
                  stats={previewData[i].stats}
                  loading={previewData[i].loading}
                  onRefresh={() => loadPreview(i, ctx.path)}
                />
              )}
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              setFormData({
                ...formData,
                contexts: [...(formData.contexts || []), { type: "folder", path: "" }],
              });
            }}
          >
            <Plus className="w-3 h-3 mr-1" />
            Add Folder
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setShowRemoteTemplates(true)}
          >
            <Download className="w-3 h-3 mr-1" />
            Browse Remote Templates
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          Folder contents will be scanned and attached to runs created by this assistant.
        </p>
      </div>
      
      {/* Remote Template Browser */}
      <RemoteTemplateBrowser
        open={showRemoteTemplates}
        onOpenChange={setShowRemoteTemplates}
      />
      
      {/* Plugin connection bindings */}
      <div className="space-y-2">
        <div>
          <h3 className="text-sm font-semibold">Bound connections</h3>
          <p className="text-xs text-muted-foreground">
            Pick which plugin connections this assistant can use, and in what role.
            Connections are configured once in Settings → Plugins.
          </p>
        </div>
        {assistant?.id ? (
          <AssistantBindingsPanel assistantID={assistant.id} />
        ) : (
          <div className="rounded-md border border-dashed p-3 text-sm text-muted-foreground">
            Save the assistant first, then bind plugin connections here.
          </div>
        )}
      </div>

      {submitError && typeof submitError !== "string" && submitError.code === "ceiling_violation" && (
        <div className="rounded border border-amber-300 bg-amber-50 dark:border-amber-700 dark:bg-amber-950 p-3 text-sm space-y-2">
          <div className="font-medium text-amber-900 dark:text-amber-200">
            This policy has rules that won&apos;t take effect
          </div>
          <ul className="list-disc list-inside text-amber-900 dark:text-amber-200">
            {submitError.violations.map((v) => (
              <li key={v.capability}>
                <code className="font-mono">{v.capability}</code> needs the{" "}
                <strong>{v.family}</strong> capability family declared above
              </li>
            ))}
          </ul>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => applySuggestedCapabilities(submitError.suggested_capabilities)}
          >
            Add missing families
          </Button>
        </div>
      )}
      {submitError && typeof submitError === "string" && (
        <div className="rounded border border-red-300 bg-red-50 dark:border-red-700 dark:bg-red-950 p-3 text-sm text-red-900 dark:text-red-200">
          {submitError}
        </div>
      )}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
        <Button
          type="submit"
          disabled={ruleCeilingViolations.length > 0}
          title={
            ruleCeilingViolations.length > 0
              ? "Fix the capability ceiling violations before saving"
              : undefined
          }
        >
          {assistant ? "Update" : "Create"} Assistant
        </Button>
      </DialogFooter>
    </form>
  );
}

function toSubmitError(error: unknown): string | CeilingViolationError {
  if (error instanceof ApiError && error.body && error.body.code === "ceiling_violation") {
    return {
      code: "ceiling_violation",
      message: error.message,
      violations: (error.body.violations as CeilingViolation[]) ?? [],
      suggested_capabilities: (error.body.suggested_capabilities as string[]) ?? [],
    };
  }
  return error instanceof Error ? error.message : String(error);
}

export function AssistantManager() {
  const [assistants, setAssistants] = useState<Assistant[]>([]);
  const [loading, setLoading] = useState(true);
  const [editingAssistant, setEditingAssistant] = useState<Assistant | null>(null);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [deleteTargetId, setDeleteTargetId] = useState<string | null>(null);
  const [submitError, setSubmitError] = useState<string | CeilingViolationError | null>(null);

  useEffect(() => {
    loadAssistants();
  }, []);

  const loadAssistants = async () => {
    try {
      const data = await assistantsApi.list();
      setAssistants(data.assistants);
    } catch (error) {
      console.error("Failed to load assistants:", error);
    } finally {
      setLoading(false);
    }
  };

  const handleCreate = async (data: CreateAssistantRequest) => {
    setSubmitError(null);
    try {
      await assistantsApi.create(data);
      setDialogOpen(false);
      loadAssistants();
    } catch (error) {
      setSubmitError(toSubmitError(error));
    }
  };

  const handleUpdate = async (data: CreateAssistantRequest) => {
    if (!editingAssistant) return;
    setSubmitError(null);
    try {
      await assistantsApi.update(editingAssistant.id, data);
      setEditingAssistant(null);
      setDialogOpen(false);
      loadAssistants();
    } catch (error) {
      setSubmitError(toSubmitError(error));
    }
  };

  const requestDelete = (id: string) => {
    setDeleteTargetId(id);
  };

  const confirmDelete = async () => {
    if (!deleteTargetId) return;
    try {
      await assistantsApi.delete(deleteTargetId);
      loadAssistants();
    } catch (error) {
      console.error("Failed to delete assistant:", error);
    }
  };

  const handleEdit = (assistant: Assistant) => {
    setEditingAssistant(assistant);
    setSubmitError(null);
    setDialogOpen(true);
  };

  if (loading) {
    return <div className="p-4">Loading assistants...</div>;
  }

  return (
    <div className="p-4 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Assistants</h2>
        <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
          <DialogTrigger asChild>
            <Button
              onClick={() => {
                setEditingAssistant(null);
                setSubmitError(null);
                setDialogOpen(true);
              }}
            >
              Create Assistant
            </Button>
          </DialogTrigger>
          <DialogContent className="max-w-2xl max-h-[90vh] overflow-y-auto">
            <DialogHeader>
              <DialogTitle>{editingAssistant ? "Edit Assistant" : "Create Assistant"}</DialogTitle>
            </DialogHeader>
            <AssistantForm
              assistant={editingAssistant || undefined}
              onSubmit={editingAssistant ? handleUpdate : handleCreate}
              onCancel={() => {
                setEditingAssistant(null);
                setSubmitError(null);
                setDialogOpen(false);
              }}
              submitError={submitError}
            />
          </DialogContent>
        </Dialog>
      </div>

      {assistants.length === 0 ? (
        <div className="text-muted-foreground">No assistants yet</div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {assistants.map((assistant) => (
            <AssistantCard key={assistant.id} assistant={assistant} onEdit={handleEdit} onDelete={requestDelete} />
          ))}
        </div>
      )}

      <ConfirmDialog
        open={deleteTargetId !== null}
        onOpenChange={(next) => !next && setDeleteTargetId(null)}
        title="Delete assistant?"
        description={`This removes the assistant and any in-flight ${labels.entity.run.plural} that depend on it will fail. Memory entries tied to it are kept but no longer associated.`}
        confirmLabel="Delete"
        destructive
        onConfirm={confirmDelete}
      />
    </div>
  );
}
