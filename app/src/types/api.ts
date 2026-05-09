export type RunStatus =
  | "created"
  | "planning"
  | "plan_review"
  | "awaiting_approval"
  | "executing"
  | "paused"
  | "completed"
  | "failed"
  | "cancelled";

export type StepStatus =
  | "pending"
  | "ready"
  | "running"
  | "retrying"
  | "blocked"
  | "done"
  | "failed";

export interface Run {
  id: string;
  goal: string;
  assistant_id: string;
  source?: string;
  conversation_id?: string;
  status: RunStatus;
  current_step_id?: string;
  plan_version: number;
  run_parent_id?: string;
  branched_from_step_id?: string;
  created_at: string;
  updated_at: string;
}

// Plugin architecture types (ADR 0001). Mirror the Go types in
// internal/plugins/manifest.go. Kept close to the wire shape so the UI
// doesn't need a translation layer.

export type ConnectionCardinality = "single" | "multi" | "multi-multi";
export type BindingRole = "channel" | "tool" | "trigger" | "context_source";
export type PermissionMode = "allow" | "confirm" | "deny";

export interface PluginManifest {
  id: string;
  name: string;
  version: string;
  author?: string;
  description?: string;
  icon_url?: string;
  cardinality: ConnectionCardinality;
  capabilities: string[];
  contributes: {
    channels?: { kind: string; description?: string; supports_threading: boolean }[];
    tools?: { name: string; capability: string; description?: string; requires_connection: boolean }[];
    triggers?: { kind: string; event_type: string; description?: string }[];
    context_sources?: { name: string; description?: string }[];
  };
  requires?: {
    credentials?: {
      kind: string;
      key: string;
      label: string;
      required: boolean;
      description?: string;
    }[];
    config_schema?: Record<string, { type: string; label: string; required: boolean; default?: string; description?: string }>;
    network_allowlist?: string[];
  };
}

// NomiHub marketplace catalog (lifecycle-09). Returned by
// GET /plugins/marketplace; entries feed the install dialog and the
// browser tab.
export interface MarketplaceEntry {
  plugin_id: string;
  name: string;
  latest_version: string;
  author?: string;
  description?: string;
  capabilities: string[];
  network_allowlist?: string[];
  install_size_bytes: number;
  sha256: string;
  bundle_url: string;
  publisher_fingerprint: string;
  published_at: string;
  readme_excerpt?: string;
}

export interface MarketplaceCatalog {
  schema_version: number;
  generated_at: string;
  entries: MarketplaceEntry[];
}

export interface PluginStatus {
  running: boolean;
  ready: boolean;
  last_error?: string;
  last_event_at?: string;
}

export interface ConnectionHealth {
  running: boolean;
  last_event_at?: string;
  last_error?: string;
  error_count: number;
}

export interface PluginConnection {
  id: string;
  plugin_id: string;
  name: string;
  config: Record<string, unknown>;
  credentials: Record<string, boolean>; // key → configured?
  enabled: boolean;
  health?: ConnectionHealth;
  webhook_url?: string;
  webhook_enabled: boolean;
  webhook_event_allowlist: string[];
  created_at: string;
  updated_at: string;
}

export type PluginDistribution = "system" | "marketplace" | "dev";

export interface PluginState {
  plugin_id: string;
  distribution: PluginDistribution;
  installed: boolean;
  enabled: boolean;
  enabled_roles?: string[];
  version?: string;
  available_version?: string;
  source_url?: string;
  signature_fingerprint?: string;
  installed_at: string;
  last_checked_at?: string;
}

export interface Plugin {
  manifest: PluginManifest;
  status: PluginStatus;
  state?: PluginState;
  connections: PluginConnection[];
}

export type FirstContactPolicy = "drop" | "reply_request_access" | "queue_approval";

export interface ChannelIdentity {
  id: string;
  plugin_id: string;
  connection_id: string;
  external_identifier: string;
  display_name: string;
  allowed_assistants: string[];
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface AssistantBinding {
  assistant_id: string;
  connection_id: string;
  role: BindingRole;
  enabled: boolean;
  is_primary: boolean;
  priority: number;
  created_at: string;
}

export interface Conversation {
  id: string;
  plugin_id: string;
  connection_id: string;
  external_conversation_id: string;
  identity_id?: string;
  assistant_id: string;
  created_at: string;
  updated_at: string;
}

export interface Step {
  id: string;
  run_id: string;
  step_definition_id?: string;
  title: string;
  status: StepStatus;
  input?: string;
  output?: string;
  error?: string;
  retry_count: number;
  created_at: string;
  updated_at: string;
}

export interface ContextAttachment {
  type: string;
  path: string;
}

export interface PermissionRule {
  capability: string;
  mode: "allow" | "confirm" | "deny";
  /**
   * Capability-specific constraints that narrow what the permission allows.
   * Interpreted by the tool the capability maps to, not by the permission
   * engine. Known shapes today:
   *
   *   command.exec → { allowed_binaries: string[] }
   *
   * Planned: filesystem.read { max_bytes }, network.outgoing { allowed_hosts }.
   */
  constraints?: Record<string, unknown>;
}

export interface PermissionPolicy {
  rules: PermissionRule[];
}

export interface MemoryPolicy {
  enabled: boolean;
  scope?: string;
  summary_template?: string;
}

export interface ChannelConfig {
  connector: string;
  connections: string[];
}

export interface RecommendedBinding {
  plugin_id: string;
  role: BindingRole;
  reason: string;
}

export interface Assistant {
  id: string;
  template_id?: string;
  name: string;
  tagline?: string;
  role: string;
  best_for?: string;
  not_for?: string;
  suggested_model?: string;
  system_prompt: string;
  channels?: string[];
  channel_configs?: ChannelConfig[];
  capabilities?: string[];
  contexts?: ContextAttachment[];
  memory_policy?: MemoryPolicy;
  permission_policy?: PermissionPolicy;
  model_policy?: ModelPolicy;
  recommended_bindings?: RecommendedBinding[];
  created_at: string;
}

export interface Event {
  id: string;
  type: string;
  run_id: string;
  step_id?: string;
  payload?: Record<string, unknown>;
  timestamp: string;
}

export interface Approval {
  id: string;
  run_id: string;
  step_id?: string;
  capability: string;
  context?: Record<string, unknown>;
  status: "pending" | "approved" | "denied";
  created_at: string;
}

export interface StepDefinition {
  id: string;
  plan_id: string;
  title: string;
  description?: string;
  expected_tool?: string;
  expected_capability?: string;
  depends_on?: string[];
  why?: string;  // Why this step was planned (e.g., "Based on your preference for...")
  arguments?: Record<string, unknown>;  // Planner-supplied tool args; e.g. {diff} for filesystem.patch.
  order: number;
  created_at: string;
}

export interface Plan {
  id: string;
  run_id: string;
  version: number;
  steps: StepDefinition[];
  created_at: string;
}

export interface RunWithSteps {
  run: Run;
  steps: Step[];
  plan?: Plan;
}

export interface CreateRunRequest {
  goal: string;
  assistant_id: string;
}

export interface CreateAssistantRequest {
  name: string;
  template_id?: string;
  tagline?: string;
  role: string;
  best_for?: string;
  not_for?: string;
  suggested_model?: string;
  system_prompt: string;
  channels?: string[];
  channel_configs?: ChannelConfig[];
  capabilities?: string[];
  contexts?: ContextAttachment[];
  memory_policy?: MemoryPolicy;
  permission_policy?: PermissionPolicy;
  model_policy?: ModelPolicy;
}

export interface Memory {
  id: string;
  scope: string;
  content: string;
  assistant_id?: string;
  run_id?: string;
  created_at: string;
}

export interface CreateMemoryRequest {
  content: string;
  scope?: string;
  assistant_id?: string;
  run_id?: string;
}

export interface ConnectorManifest {
  id: string;
  name: string;
  version: string;
  description: string;
  author?: string;
  permissions: string[];
  config_schema?: Record<string, ConfigField>;
}

export interface ConfigField {
  type: string;
  label: string;
  required: boolean;
  default?: string;
  description?: string;
}

export interface ConnectorStatus {
  name: string;
  enabled: boolean;
  running: boolean;
  last_error?: string;
}

/**
 * Provider profile as returned by the backend. The raw `secret_ref` column
 * (which holds a `secret://…` URI after migration) is deliberately not on
 * the wire. The UI sees only a boolean telling it whether a credential is
 * configured.
 */
export interface ProviderProfile {
  id: string;
  name: string;
  type: "local" | "remote";
  endpoint?: string;
  model_ids: string[];
  secret_configured: boolean;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

/**
 * Request body for create/update. `secret_ref` here is the plaintext API key
 * the user entered. The backend stashes it in the OS keyring (or encrypted
 * file vault) and persists only the reference. Leave undefined on update to
 * keep the existing secret unchanged.
 */
export interface ProviderProfileRequest {
  name: string;
  type: "local" | "remote";
  endpoint?: string;
  model_ids: string[];
  secret_ref?: string;
  enabled: boolean;
}

export interface ModelPolicy {
  mode: "global_default" | "assistant_override";
  preferred?: string;
  fallback?: string;
  local_only?: boolean;
  allow_fallback?: boolean;
}

export interface LLMDefaultSettings {
  provider_id: string;
  model_id: string;
}

export interface OnboardingStatus {
  complete: boolean;
}

export type SafetyProfile = "cautious" | "balanced" | "fast";

export interface SafetyProfileSettings {
  profile: SafetyProfile;
}

export interface ConnectorConfig {
  name: string;
  manifest: ConnectorManifest;
  status: ConnectorStatus;
  config: Record<string, unknown>;
  enabled: boolean;
}

export interface TriggerRule {
  name: string;
  assistant_id: string;
  from_contains?: string;
  subject_contains?: string;
  body_contains?: string;
  enabled: boolean;
}

export interface RemoteTemplate {
  id: string;
  catalog_hash?: string;
  source_url?: string;
  signature?: string;
  name: string;
  tagline?: string;
  role?: string;
  best_for?: string;
  not_for?: string;
  suggest_ed_model?: string;
  system_prompt?: string;
  channels?: string[]; // JSON array
  capabilities?: string[]; // JSON array
  contexts?: any[]; // JSON array
  memory_policy?: any; // JSON object
  permission_policy?: any; // JSON object
  recommended_bindings?: any[]; // JSON array
  installed_at?: string;
  local_assistant_id?: string;
}

export interface ApiError {
  error: string;
}
