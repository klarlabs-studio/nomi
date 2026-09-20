// Popular MCP server presets for the generic MCP plugin (com.nomi.mcp).
// Mirrors email-presets.ts: fill the Add-connection form so Goose-style
// "pick a server, tweak the path" beats hand-writing npx/uvx lines.
//
// Commands match the official modelcontextprotocol/servers docs. Args are
// stored as a single string; nomid splits on commas or whitespace
// (see internal/plugins/mcpbridge.splitArgs). Prefer space-separated
// tokens that do not contain spaces themselves.
//
// Secrets for GitHub / Postgres go through envCredentials → credential_refs
// and are injected at spawn (never into SQLite config plaintext).

export type McpTransport = "stdio" | "http";

/** Secret env var collected in the UI and stored via credential_refs. */
export interface McpEnvCredential {
  /** Environment variable name, e.g. GITHUB_PERSONAL_ACCESS_TOKEN. */
  key: string;
  label: string;
  required: boolean;
  description?: string;
}

export interface McpServerPreset {
  id: string;
  label: string;
  description: string;
  /** Pre-filled connection display name (user can edit). */
  suggestedName: string;
  transport: McpTransport;
  command?: string;
  args?: string;
  endpoint?: string;
  /** Shown under the preset picker — path tokens, runtime deps, etc. */
  setupNote: string;
  docURL: string;
  /**
   * When true, Create can run without further edits (memory / fetch /
   * time). Path-scoped presets stay false so the user replaces the
   * placeholder before submit. Env-secret presets stay false until
   * the secret field is filled (enforced in the dialog).
   */
  readyToCreate: boolean;
  /** Secret env vars for stdio servers (PAT, DATABASE_URL, …). */
  envCredentials?: McpEnvCredential[];
}

export const MCP_SERVER_PRESETS: McpServerPreset[] = [
  {
    id: "filesystem",
    label: "Filesystem",
    description: "Read/write files under an allowlisted directory.",
    suggestedName: "Filesystem",
    transport: "stdio",
    command: "npx",
    args: "-y @modelcontextprotocol/server-filesystem /path/to/allowed",
    setupNote:
      "Replace /path/to/allowed with a directory Nomi may touch. Requires Node.js + npx.",
    docURL:
      "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
    readyToCreate: false,
  },
  {
    id: "memory",
    label: "Memory",
    description: "Knowledge-graph scratch memory across turns.",
    suggestedName: "Memory",
    transport: "stdio",
    command: "npx",
    args: "-y @modelcontextprotocol/server-memory",
    setupNote: "Requires Node.js + npx. No extra config.",
    docURL:
      "https://github.com/modelcontextprotocol/servers/tree/main/src/memory",
    readyToCreate: true,
  },
  {
    id: "fetch",
    label: "Fetch",
    description: "Fetch URLs and convert HTML to markdown.",
    suggestedName: "Fetch",
    transport: "stdio",
    command: "uvx",
    args: "mcp-server-fetch",
    setupNote: "Requires uv (Astral). Alternative: pip install mcp-server-fetch.",
    docURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
    readyToCreate: true,
  },
  {
    id: "git",
    label: "Git",
    description: "Inspect and manipulate a local Git repository.",
    suggestedName: "Git",
    transport: "stdio",
    command: "uvx",
    args: "mcp-server-git --repository /path/to/repo",
    setupNote: "Replace /path/to/repo with the repository root. Requires uv.",
    docURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/git",
    readyToCreate: false,
  },
  {
    id: "github",
    label: "GitHub",
    description: "GitHub issues, PRs, and repo tools via the official MCP server.",
    suggestedName: "GitHub",
    transport: "stdio",
    command: "docker",
    args: "run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server",
    setupNote:
      "Requires Docker. Paste a fine-scoped PAT below — stored in the OS secret store and injected as GITHUB_PERSONAL_ACCESS_TOKEN at spawn.",
    docURL: "https://github.com/github/github-mcp-server",
    readyToCreate: false,
    envCredentials: [
      {
        key: "GITHUB_PERSONAL_ACCESS_TOKEN",
        label: "GitHub personal access token",
        required: true,
        description: "PAT with the scopes your tools need. Never stored in SQLite config.",
      },
    ],
  },
  {
    id: "postgres",
    label: "PostgreSQL",
    description: "Read-only SQL against a Postgres database.",
    suggestedName: "Postgres",
    transport: "stdio",
    command: "npx",
    args: "-y @modelcontextprotocol/server-postgres ${DATABASE_URL}",
    setupNote:
      "Requires Node.js + npx. Connection URL is a secret env var — expanded into args at spawn, not written into config.",
    docURL:
      "https://github.com/modelcontextprotocol/servers/tree/main/src/postgres",
    readyToCreate: false,
    envCredentials: [
      {
        key: "DATABASE_URL",
        label: "PostgreSQL connection URL",
        required: true,
        description: "postgresql://user:pass@host:5432/db — stored as a secret, expanded via ${DATABASE_URL}.",
      },
    ],
  },
  {
    id: "time",
    label: "Time",
    description: "Time and timezone conversion helpers.",
    suggestedName: "Time",
    transport: "stdio",
    command: "uvx",
    args: "mcp-server-time",
    setupNote: "Requires uv. No extra config.",
    docURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/time",
    readyToCreate: true,
  },
  {
    id: "sequential-thinking",
    label: "Sequential Thinking",
    description: "Structured multi-step reasoning tool.",
    suggestedName: "Sequential Thinking",
    transport: "stdio",
    command: "npx",
    args: "-y @modelcontextprotocol/server-sequential-thinking",
    setupNote: "Requires Node.js + npx. No extra config.",
    docURL:
      "https://github.com/modelcontextprotocol/servers/tree/main/src/sequentialthinking",
    readyToCreate: true,
  },
  {
    id: "http-remote",
    label: "Remote HTTP / SSE",
    description: "Any MCP server that speaks HTTP+SSE.",
    suggestedName: "Remote MCP",
    transport: "http",
    endpoint: "https://mcp.example.com/sse",
    setupNote:
      "Replace the endpoint with your server URL. Optional bearer token goes in the credential field below.",
    docURL: "https://modelcontextprotocol.io/docs/concepts/transports",
    readyToCreate: false,
  },
  {
    id: "custom",
    label: "Custom (manual)",
    description: "Blank form — enter command/args or endpoint yourself.",
    suggestedName: "",
    transport: "stdio",
    command: "",
    args: "",
    setupNote:
      "Point Nomi at any MCP server. Discovered tools register as mcp.<name>.<tool> and still go through plan review. Use Environment secrets for PATs / connection strings.",
    docURL: "https://modelcontextprotocol.io/examples",
    readyToCreate: false,
  },
];

/** Apply a preset onto the connection config form (string values). */
export function applyMcpPresetConfig(
  preset: McpServerPreset,
  prev: Record<string, string>,
): Record<string, string> {
  const next: Record<string, string> = {
    ...prev,
    transport: preset.transport,
  };
  if (preset.transport === "http") {
    next.command = "";
    next.args = "";
    next.endpoint = preset.endpoint ?? "";
    next.env = "";
  } else {
    next.command = preset.command ?? "";
    next.args = preset.args ?? "";
    next.endpoint = "";
    // Keep any user-typed env literals unless switching away from custom.
  }
  return next;
}

export function findMcpPreset(id: string): McpServerPreset | undefined {
  return MCP_SERVER_PRESETS.find((p) => p.id === id);
}

/** Parse KEY=value lines into a config.env object for the API. */
export function parseEnvLiteralLines(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const i = line.indexOf("=");
    if (i <= 0) continue;
    const key = line.slice(0, i).trim();
    const val = line.slice(i + 1).trim();
    if (key) out[key] = val;
  }
  return out;
}
