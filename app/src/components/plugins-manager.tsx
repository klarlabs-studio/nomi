import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { pluginsApi } from "@/lib/api";
import { errorMessage } from "@/lib/utils";
import type { Plugin, PluginConnection, PluginManifest } from "@/types/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { ToggleSwitch } from "@/components/ui/toggle-switch";
import {
  Plug,
  RefreshCw,
  Plus,
  Trash2,
  Check,
  Eye,
  EyeOff,
  ChevronDown,
  ChevronRight,
  AlertCircle,
  Circle,
  Download,
  ArrowUpCircle,
  Store,
} from "lucide-react";
import { IdentityAllowlist } from "@/components/identity-allowlist";
import { TriggerRulesEditor } from "@/components/trigger-rules-editor";
import { InstallPluginDialog } from "@/components/install-plugin-dialog";
import { MarketplaceBrowserDialog } from "@/components/marketplace-browser-dialog";
import {
  EMAIL_PROVIDER_PRESETS,
  detectProviderFromEmail,
  type EmailProviderPreset,
} from "@/lib/email-presets";

const EMAIL_PLUGIN_ID = "com.nomi.email";

// HealthBadge renders the per-connection health indicator. Rule of thumb:
//   - red dot   → LastError is set (most recent op failed)
//   - green dot → Running + LastEventAt within last 2 minutes
//   - amber dot → Running but no activity in >2 minutes (connection is up
//                 but quiet; usually fine, occasionally an early signal)
//   - grey dot  → plugin returned no health info, or Running is false
function HealthBadge({ health }: { health?: PluginConnection["health"] }) {
  if (!health) {
    return (
      <span className="inline-flex items-center gap-1 text-[10px] text-muted-foreground">
        <Circle className="w-2 h-2 fill-current" /> unknown
      </span>
    );
  }
  if (health.last_error) {
    return (
      <span
        className="inline-flex items-center gap-1 text-[10px] text-destructive"
        title={`${health.last_error} (${health.error_count} consecutive errors)`}
      >
        <AlertCircle className="w-3 h-3" /> error
      </span>
    );
  }
  if (!health.running) {
    return (
      <span className="inline-flex items-center gap-1 text-[10px] text-muted-foreground">
        <Circle className="w-2 h-2 fill-current" /> stopped
      </span>
    );
  }
  const lastEvent = health.last_event_at ? new Date(health.last_event_at).getTime() : 0;
  const ageMs = lastEvent ? Date.now() - lastEvent : Number.POSITIVE_INFINITY;
  if (ageMs < 2 * 60 * 1000) {
    return (
      <span className="inline-flex items-center gap-1 text-[10px] text-green-700">
        <Circle className="w-2 h-2 fill-current" /> healthy
      </span>
    );
  }
  return (
    <span
      className="inline-flex items-center gap-1 text-[10px] text-amber-700"
      title={lastEvent ? `Last activity: ${new Date(lastEvent).toLocaleTimeString()}` : undefined}
    >
      <Circle className="w-2 h-2 fill-current" /> idle
    </span>
  );
}

// Plugins tab (ADR 0001). Replaces the legacy Connections-tab connector
// list. Pulls from the new /plugins endpoint which surfaces the full
// PluginManifest plus the live connection set, so this component renders
// per-plugin cards with:
//   - Manifest metadata (name, version, author, description)
//   - Roles it plays (channel / tool / trigger / context source)
//   - Connections list with per-connection enable toggle + delete
//   - "Add connection" dialog that collects credentials based on the
//     manifest's Requires.Credentials descriptor

function RolesBadges({ manifest }: { manifest: PluginManifest }) {
  const roles: string[] = [];
  if ((manifest.contributes.channels?.length ?? 0) > 0) roles.push("channel");
  if ((manifest.contributes.tools?.length ?? 0) > 0) roles.push("tool");
  if ((manifest.contributes.triggers?.length ?? 0) > 0) roles.push("trigger");
  if ((manifest.contributes.context_sources?.length ?? 0) > 0) roles.push("context");
  return (
    <div className="flex flex-wrap gap-1">
      {roles.map((r) => (
        <Badge key={r} variant="outline" className="text-[10px]">
          {r}
        </Badge>
      ))}
    </div>
  );
}

// initialConfig pre-fills a connection form with the defaults declared in
// the plugin's manifest. Number fields show as strings in the input and
// are coerced back to numbers during submit.
function initialConfig(plugin: Plugin): Record<string, string> {
  const out: Record<string, string> = {};
  const schema = plugin.manifest.requires?.config_schema;
  if (!schema) return out;
  for (const [key, field] of Object.entries(schema)) {
    if (field.default !== undefined) {
      out[key] = field.default;
    } else {
      out[key] = "";
    }
  }
  return out;
}

function AddConnectionDialog({
  plugin,
  onClose,
}: {
  plugin: Plugin;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const isEmail = plugin.manifest.id === EMAIL_PLUGIN_ID;

  const [name, setName] = useState("");
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [showSecret, setShowSecret] = useState<Record<string, boolean>>({});
  const [config, setConfig] = useState<Record<string, string>>(() =>
    initialConfig(plugin),
  );
  const [error, setError] = useState<string | null>(null);
  const [preset, setPreset] = useState<EmailProviderPreset | null>(
    isEmail ? EMAIL_PROVIDER_PRESETS[0] : null,
  );

  const applyPreset = (p: EmailProviderPreset) => {
    setPreset(p);
    setConfig((prev) => ({
      ...prev,
      imap_host: p.imapHost,
      imap_port: String(p.imapPort),
      smtp_host: p.smtpHost,
      smtp_port: String(p.smtpPort),
    }));
  };

  const create = useMutation({
    mutationFn: () => {
      const configPayload: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(config)) {
        if (v === "") continue;
        // Pass numeric-looking fields as numbers to match the Go config shape.
        const field = plugin.manifest.requires?.config_schema?.[k];
        if (field?.type === "number") {
          const n = Number(v);
          configPayload[k] = Number.isFinite(n) ? n : v;
        } else {
          configPayload[k] = v;
        }
      }
      return pluginsApi.createConnection(plugin.manifest.id, {
        name: name.trim(),
        config: configPayload,
        credentials,
        enabled: true,
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["plugins"] });
      onClose();
    },
    onError: (err) => setError(errorMessage(err)),
  });

  const requiredCreds = plugin.manifest.requires?.credentials ?? [];
  const configSchema = Object.entries(plugin.manifest.requires?.config_schema ?? {});

  // Email-only: when the user types their email into the username field,
  // try to auto-detect the provider preset. Firing on every keystroke
  // would churn the form; throttle via a tolerance check.
  const onUsernameChange = (value: string) => {
    setConfig((prev) => ({ ...prev, username: value }));
    if (!isEmail) return;
    const detected = detectProviderFromEmail(value);
    if (detected.id !== "generic" && detected.id !== preset?.id) {
      applyPreset(detected);
    }
  };

  return (
    <div className="border rounded-md p-3 space-y-3 bg-background">
      <div className="space-y-1">
        <label className="text-sm font-medium">Display name</label>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={`${plugin.manifest.name} account`}
        />
      </div>

      {isEmail && (
        <div className="space-y-1">
          <label className="text-sm font-medium">Email provider</label>
          <select
            className="w-full text-sm border rounded px-2 py-1 bg-background"
            value={preset?.id ?? "generic"}
            onChange={(e) => {
              const p = EMAIL_PROVIDER_PRESETS.find((x) => x.id === e.target.value);
              if (p) applyPreset(p);
            }}
          >
            {EMAIL_PROVIDER_PRESETS.map((p) => (
              <option key={p.id} value={p.id}>
                {p.label}
              </option>
            ))}
          </select>
          {preset && preset.id !== "generic" && (
            <p className="text-xs text-muted-foreground">
              {preset.authNote}
              {preset.docURL && (
                <>
                  {" "}
                  <a
                    href={preset.docURL}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="underline"
                  >
                    Setup guide
                  </a>
                </>
              )}
            </p>
          )}
        </div>
      )}

      {configSchema.map(([key, field]) => {
        const isUsername = isEmail && key === "username";
        return (
          <div key={key} className="space-y-1">
            <label className="text-sm font-medium">
              {field.label}
              {field.required && <span className="text-destructive ml-1">*</span>}
            </label>
            {field.description && (
              <p className="text-xs text-muted-foreground">{field.description}</p>
            )}
            {field.type === "boolean" ? (
              <label className="inline-flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={config[key] === "true"}
                  onChange={(e) =>
                    setConfig((prev) => ({ ...prev, [key]: e.target.checked ? "true" : "false" }))
                  }
                />
                Enabled
              </label>
            ) : (
              <Input
                type={field.type === "number" ? "number" : "text"}
                value={config[key] ?? ""}
                onChange={(e) =>
                  isUsername ? onUsernameChange(e.target.value) : setConfig((prev) => ({ ...prev, [key]: e.target.value }))
                }
                placeholder={field.default ?? ""}
              />
            )}
          </div>
        );
      })}

      {requiredCreds.map((cred) => {
        const visible = showSecret[cred.key];
        // PEM blobs are multi-line by nature; collapse the masked
        // single-line input into a textarea while keeping the
        // show/hide toggle for safety.
        const isMultiline = cred.kind === "github_app_private_key";
        return (
          <div key={cred.key} className="space-y-1">
            <label className="text-sm font-medium">
              {cred.label}
              {cred.required && <span className="text-destructive ml-1">*</span>}
            </label>
            {cred.description && (
              <p className="text-xs text-muted-foreground">{cred.description}</p>
            )}
            <div className="relative">
              {isMultiline ? (
                <textarea
                  className={`w-full text-sm border rounded px-2 py-1 bg-background font-mono ${
                    visible ? "" : "blur-sm"
                  }`}
                  rows={8}
                  spellCheck={false}
                  value={credentials[cred.key] ?? ""}
                  onChange={(e) =>
                    setCredentials((prev) => ({ ...prev, [cred.key]: e.target.value }))
                  }
                  placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;...&#10;-----END RSA PRIVATE KEY-----"
                />
              ) : (
                <Input
                  type={visible ? "text" : "password"}
                  value={credentials[cred.key] ?? ""}
                  onChange={(e) =>
                    setCredentials((prev) => ({ ...prev, [cred.key]: e.target.value }))
                  }
                  placeholder={cred.kind === "bot_token" ? "123456:ABC-..." : ""}
                />
              )}
              <button
                type="button"
                onClick={() =>
                  setShowSecret((prev) => ({ ...prev, [cred.key]: !prev[cred.key] }))
                }
                className="absolute right-2 top-2 text-muted-foreground hover:text-foreground"
              >
                {visible ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            </div>
          </div>
        );
      })}
      {error && <p className="text-sm text-destructive">{error}</p>}
      <div className="flex justify-end gap-2">
        <Button size="sm" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button
          size="sm"
          onClick={() => {
            if (!name.trim()) {
              setError("Name is required");
              return;
            }
            for (const [key, field] of configSchema) {
              if (field.required && !config[key]) {
                setError(`${field.label} is required`);
                return;
              }
            }
            for (const cred of requiredCreds) {
              if (cred.required && !credentials[cred.key]) {
                setError(`${cred.label} is required`);
                return;
              }
            }
            create.mutate();
          }}
          disabled={create.isPending}
        >
          {create.isPending ? "Adding..." : "Add connection"}
        </Button>
      </div>
    </div>
  );
}

function ConnectionRow({
  plugin,
  connection,
}: {
  plugin: Plugin;
  connection: PluginConnection;
}) {
  const qc = useQueryClient();
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const hasChannelRole = (plugin.manifest.contributes.channels?.length ?? 0) > 0;
  const hasWebhookSupport = (plugin.manifest.contributes.triggers?.length ?? 0) > 0;

  const toggle = useMutation({
    mutationFn: (enabled: boolean) =>
      pluginsApi.updateConnection(plugin.manifest.id, connection.id, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const remove = useMutation({
    mutationFn: () => pluginsApi.deleteConnection(plugin.manifest.id, connection.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const rotateSecret = useMutation({
    mutationFn: () => pluginsApi.rotateWebhookSecret(connection.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const updateAllowlist = useMutation({
    mutationFn: (allowlist: string[]) =>
      pluginsApi.updateWebhookAllowlist(connection.id, allowlist),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const credentialSummary = Object.entries(connection.credentials)
    .map(([key, configured]) => `${key}: ${configured ? "set" : "missing"}`)
    .join(", ");

  const toggleWebhook = useMutation({
    mutationFn: (webhookEnabled: boolean) =>
      pluginsApi.updateConnection(plugin.manifest.id, connection.id, {
        webhook_enabled: webhookEnabled,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  return (
    <div className="border rounded-md text-sm">
      <div className="flex items-center justify-between p-2">
        <div className="min-w-0 flex items-center gap-2">
          {(hasChannelRole || hasWebhookSupport) && (
            <button
              onClick={() => setExpanded((v) => !v)}
              className="text-muted-foreground hover:text-foreground"
              aria-label="Toggle details"
              type="button"
            >
              {expanded ? (
                <ChevronDown className="w-4 h-4" />
              ) : (
                <ChevronRight className="w-4 h-4" />
              )}
            </button>
          )}
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className="font-medium truncate">{connection.name}</span>
              {connection.enabled ? (
                <Badge variant="default" className="text-[10px]">
                  enabled
                </Badge>
              ) : (
                <Badge variant="secondary" className="text-[10px]">
                  disabled
                </Badge>
              )}
              <HealthBadge health={connection.health} />
            </div>
            <div className="text-xs text-muted-foreground mt-0.5 truncate">
              <code className="font-mono">{connection.id.slice(0, 8)}</code>
              {credentialSummary && <> · {credentialSummary}</>}
            </div>
          </div>
        </div>
        <div className="flex items-center gap-1">
          <ToggleSwitch
            checked={connection.enabled}
            onChange={(c) => toggle.mutate(c)}
            disabled={toggle.isPending}
          />
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              if (confirmingDelete) {
                remove.mutate();
                setConfirmingDelete(false);
              } else {
                setConfirmingDelete(true);
                setTimeout(() => setConfirmingDelete(false), 3000);
              }
            }}
            disabled={remove.isPending}
            className={confirmingDelete ? "text-destructive" : ""}
          >
            {confirmingDelete ? <Check className="w-4 h-4" /> : <Trash2 className="w-4 h-4" />}
          </Button>
        </div>
      </div>
      {expanded && (
        <div className="border-t p-2 space-y-3">
          {hasChannelRole && (
            <IdentityAllowlist pluginID={plugin.manifest.id} connectionID={connection.id} />
          )}
          {plugin.manifest.id === EMAIL_PLUGIN_ID && (
            <TriggerRulesEditor pluginID={plugin.manifest.id} connectionID={connection.id} />
          )}
          {hasWebhookSupport && (
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium">Inbound Webhooks</span>
                <ToggleSwitch
                  checked={connection.webhook_enabled}
                  onChange={(c) => toggleWebhook.mutate(c)}
                  disabled={toggleWebhook.isPending}
                />
              </div>
              {connection.webhook_enabled && (
                <div className="space-y-2">
                  {connection.webhook_url && (
                    <div className="text-xs">
                      <span className="text-muted-foreground">URL: </span>
                      <code className="font-mono bg-muted px-1 rounded">{connection.webhook_url}</code>
                    </div>
                  )}
                  <div className="flex items-center gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => rotateSecret.mutate()}
                      disabled={rotateSecret.isPending}
                      className="text-xs h-7"
                    >
                      <RefreshCw className="w-3 h-3 mr-1" />
                      Rotate Secret
                    </Button>
                  </div>
                  <div>
                    <label className="text-xs text-muted-foreground block mb-1">Event allowlist (empty = all)</label>
                    <input
                      type="text"
                      className="w-full text-xs border rounded px-2 py-1 bg-background"
                      placeholder="issues, pull_request, push..."
                      defaultValue={connection.webhook_event_allowlist.join(", ")}
                      onBlur={(e) => {
                        const vals = e.target.value
                          .split(",")
                          .map((s) => s.trim())
                          .filter((s) => s.length > 0);
                        updateAllowlist.mutate(vals);
                      }}
                    />
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function PluginCard({ plugin }: { plugin: Plugin }) {
  const [adding, setAdding] = useState(false);
  const [confirmingUninstall, setConfirmingUninstall] = useState(false);
  const [cascadeUninstall, setCascadeUninstall] = useState(false);
  const qc = useQueryClient();
  const enabled = plugin.state?.enabled ?? true;
  const isSystem = plugin.state?.distribution === "system";
  const isMarketplace = plugin.state?.distribution === "marketplace";
  const isDev = plugin.state?.distribution === "dev";
  const isUninstallable = isMarketplace || isDev;
  const updateAvailable = plugin.state?.available_version;
  const [updateError, setUpdateError] = useState<string | null>(null);
  const [uninstallError, setUninstallError] = useState<string | null>(null);

  const toggle = useMutation({
    mutationFn: (next: boolean) => pluginsApi.setEnabled(plugin.manifest.id, next),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const enabledRoles = useMemo(
    () => new Set(plugin.state?.enabled_roles ?? []),
    [plugin.state?.enabled_roles],
  );
  const allRoles = useMemo(() => {
    const out: string[] = [];
    if ((plugin.manifest.contributes.channels?.length ?? 0) > 0) out.push("channel");
    if ((plugin.manifest.contributes.tools?.length ?? 0) > 0) out.push("tool");
    if ((plugin.manifest.contributes.triggers?.length ?? 0) > 0) out.push("trigger");
    if ((plugin.manifest.contributes.context_sources?.length ?? 0) > 0) out.push("context_source");
    return out;
  }, [plugin.manifest.contributes]);

  const rolesToggle = useMutation({
    mutationFn: (roles: string[]) => pluginsApi.setEnabledRoles(plugin.manifest.id, roles),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plugins"] }),
  });

  const toggleRole = (role: string, on: boolean) => {
    const next = on
      ? [...enabledRoles, role]
      : enabledRoles.has(role)
        ? Array.from(enabledRoles).filter((r) => r !== role)
        : Array.from(enabledRoles);
    rolesToggle.mutate(next);
  };

  const update = useMutation({
    mutationFn: () => pluginsApi.update(plugin.manifest.id),
    onSuccess: () => {
      setUpdateError(null);
      qc.invalidateQueries({ queryKey: ["plugins"] });
    },
    onError: (err) => setUpdateError(errorMessage(err)),
  });

  const uninstall = useMutation({
    mutationFn: () => pluginsApi.uninstall(plugin.manifest.id, cascadeUninstall),
    onSuccess: () => {
      setUninstallError(null);
      setConfirmingUninstall(false);
      qc.invalidateQueries({ queryKey: ["plugins"] });
    },
    onError: (err) => setUninstallError(errorMessage(err)),
  });

  return (
    <Card className={enabled ? "" : "opacity-60"}>
      <CardHeader className="pb-2">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 flex-wrap">
              <Plug className="w-4 h-4 text-muted-foreground" />
              <CardTitle className="text-base">{plugin.manifest.name}</CardTitle>
              <Badge variant="outline">v{plugin.manifest.version}</Badge>
              {!enabled ? (
                <Badge variant="secondary">Disabled</Badge>
              ) : plugin.status.running ? (
                <Badge variant="default">Running</Badge>
              ) : (
                <Badge variant="secondary">Stopped</Badge>
              )}
              {isSystem && (
                <Badge variant="outline" className="text-[10px]">
                  system
                </Badge>
              )}
              {isMarketplace && (
                <Badge variant="outline" className="text-[10px]">
                  marketplace
                </Badge>
              )}
              {plugin.state?.distribution === "dev" && (
                <Badge
                  variant="outline"
                  className="text-[10px] border-destructive text-destructive"
                  title="Loaded from ~/.nomi/plugins-dev/ — unsigned, development build"
                >
                  dev (unsigned)
                </Badge>
              )}
              {updateAvailable && (
                <Badge
                  variant="outline"
                  className="text-[10px] border-amber-700 text-amber-700"
                  title={`Latest catalog version: ${updateAvailable}`}
                >
                  update available · v{updateAvailable}
                </Badge>
              )}
              <RolesBadges manifest={plugin.manifest} />
            </div>
            {allRoles.length > 0 && (
              <div className="flex flex-wrap gap-3 mt-2">
                {allRoles.map((role) => {
                  const isOn = enabledRoles.has(role);
                  return (
                    <label
                      key={role}
                      className="flex items-center gap-1.5 text-xs cursor-pointer"
                    >
                      <ToggleSwitch
                        checked={isOn}
                        onChange={(on) => toggleRole(role, on)}
                        disabled={rolesToggle.isPending}
                      />
                      <span className="capitalize">{role.replace("_", " ")}</span>
                    </label>
                  );
                })}
              </div>
            )}
            {plugin.manifest.description && (
              <p className="text-xs text-muted-foreground mt-1">{plugin.manifest.description}</p>
            )}
            <p className="text-xs text-muted-foreground mt-1">
              ID: <code>{plugin.manifest.id}</code>
              {plugin.manifest.author && ` · ${plugin.manifest.author}`}
              {" · "}
              cardinality: <code>{plugin.manifest.cardinality}</code>
            </p>
            {!enabled && (
              <p className="text-xs text-amber-700 mt-1">
                Plugin disabled — assistants can&apos;t use it and it won&apos;t receive incoming
                messages. Toggle back on to resume.
              </p>
            )}
          </div>
          <div className="flex-shrink-0 flex items-center gap-2">
            {updateAvailable && (
              <Button
                size="sm"
                variant="outline"
                onClick={() => update.mutate()}
                disabled={update.isPending}
                title={`Update to v${updateAvailable}`}
              >
                <ArrowUpCircle className="w-4 h-4 mr-1" />
                {update.isPending ? "Updating…" : "Update"}
              </Button>
            )}
            {isUninstallable && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setConfirmingUninstall((v) => !v)}
                disabled={uninstall.isPending}
                title="Uninstall plugin"
                aria-label={`Uninstall ${plugin.manifest.name}`}
              >
                <Trash2 className="w-4 h-4 text-destructive" />
              </Button>
            )}
            <ToggleSwitch
              checked={enabled}
              onChange={(c) => toggle.mutate(c)}
              disabled={toggle.isPending}
            />
          </div>
        </div>
        {confirmingUninstall && (
          <div className="border border-destructive rounded-md p-3 mt-2 space-y-2">
            <p className="text-sm font-medium">Uninstall {plugin.manifest.name}?</p>
            <p className="text-xs text-muted-foreground">
              The plugin will be removed from this installation. By default your saved
              connections (accounts, credentials) are kept so a future reinstall reattaches
              them.
            </p>
            <label className="flex items-center gap-2 text-xs">
              <input
                type="checkbox"
                checked={cascadeUninstall}
                onChange={(e) => setCascadeUninstall(e.target.checked)}
                className="rounded border-gray-300 h-3.5 w-3.5"
              />
              <span>
                Also delete all connections + credentials for this plugin (cannot be undone)
              </span>
            </label>
            <div className="flex items-center justify-end gap-2 pt-1">
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  setConfirmingUninstall(false);
                  setCascadeUninstall(false);
                }}
                disabled={uninstall.isPending}
              >
                Cancel
              </Button>
              <Button
                size="sm"
                variant="destructive"
                onClick={() => uninstall.mutate()}
                disabled={uninstall.isPending}
              >
                {uninstall.isPending ? "Uninstalling…" : "Uninstall"}
              </Button>
            </div>
          </div>
        )}
        {uninstallError && (
          <div className="text-xs text-destructive border border-destructive rounded-md p-2 mt-2">
            {uninstallError}
          </div>
        )}
        {updateError && (
          <div className="text-xs text-destructive border border-destructive rounded-md p-2 mt-2">
            {updateError}
          </div>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        <div>
          <p className="text-xs font-medium text-muted-foreground mb-2">
            Connections ({plugin.connections.length})
          </p>
          {plugin.connections.length === 0 ? (
            <p className="text-sm text-muted-foreground">No connections configured yet.</p>
          ) : (
            <div className="space-y-2">
              {plugin.connections.map((conn) => (
                <ConnectionRow key={conn.id} plugin={plugin} connection={conn} />
              ))}
            </div>
          )}
        </div>

        {plugin.manifest.cardinality !== "single" &&
          (plugin.manifest.requires?.credentials ?? []).length > 0 && (
            <>
              {!adding ? (
                <Button size="sm" variant="outline" onClick={() => setAdding(true)}>
                  <Plus className="w-4 h-4 mr-1" /> Add connection
                </Button>
              ) : (
                <AddConnectionDialog plugin={plugin} onClose={() => setAdding(false)} />
              )}
            </>
          )}

        {plugin.status.last_error && (
          <div className="text-xs text-red-600 bg-red-50 border border-red-200 rounded p-2">
            {plugin.status.last_error}
          </div>
        )}

        <details className="text-xs text-muted-foreground">
          <summary className="cursor-pointer">Declared capabilities</summary>
          <div className="mt-1 space-y-0.5">
            {plugin.manifest.capabilities.map((cap) => (
              <div key={cap} className="font-mono">
                {cap}
              </div>
            ))}
          </div>
        </details>
      </CardContent>
    </Card>
  );
}

export function PluginsManager() {
  const [installOpen, setInstallOpen] = useState(false);
  const [browseOpen, setBrowseOpen] = useState(false);
  const { data, error, isFetching, refetch } = useQuery({
    queryKey: ["plugins"],
    queryFn: () => pluginsApi.list(),
    refetchInterval: 60_000, // safety net; EventProvider invalidates on plugin.* events
  });
  const { data: tunnelStatus } = useQuery({
    queryKey: ["tunnel-status"],
    queryFn: () => pluginsApi.getTunnelStatus(),
    refetchInterval: 60_000, // safety net; EventProvider invalidates on plugin.* events
  });

  const plugins = data?.plugins ?? [];

  return (
    <div className="p-4 space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Plugins</h2>
          <p className="text-sm text-muted-foreground">
            Integrations. Each plugin declares the roles it plays (channel, tool, trigger, context)
            and the connections you&apos;ve configured.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => setBrowseOpen(true)}>
            <Store className="w-4 h-4 mr-1" />
            Browse marketplace
          </Button>
          <Button size="sm" onClick={() => setInstallOpen(true)}>
            <Download className="w-4 h-4 mr-1" />
            Install plugin
          </Button>
          <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching}>
            <RefreshCw className={`w-4 h-4 mr-1 ${isFetching ? "animate-spin" : ""}`} />
            Refresh
          </Button>
        </div>
      </div>

      {tunnelStatus?.enabled && tunnelStatus.public_url && (
        <div className="bg-muted rounded-md p-3 text-sm flex items-center gap-3">
          <Plug className="w-4 h-4 text-green-600" />
          <div>
            <span className="font-medium">Tunnel active</span>
            <span className="text-muted-foreground ml-2">Public URL:</span>{" "}
            <code className="font-mono text-xs bg-background px-1 rounded">{tunnelStatus.public_url}</code>
          </div>
        </div>
      )}

      <InstallPluginDialog open={installOpen} onOpenChange={setInstallOpen} />
      <MarketplaceBrowserDialog open={browseOpen} onOpenChange={setBrowseOpen} />

      {error && (
        <div className="bg-destructive/10 text-destructive p-3 rounded-md text-sm">
          {errorMessage(error)}
        </div>
      )}

      {plugins.length === 0 ? (
        <div className="text-muted-foreground">No plugins registered.</div>
      ) : (
        <div className="space-y-4 max-w-3xl">
          {plugins.map((plugin) => (
            <PluginCard key={plugin.manifest.id} plugin={plugin} />
          ))}
        </div>
      )}
    </div>
  );
}
