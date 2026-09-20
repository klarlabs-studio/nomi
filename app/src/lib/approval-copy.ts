export type DangerSignal = "irreversible" | "network" | "shell";

export interface ApprovalCopy {
  summary: string;
  dangerSignal?: DangerSignal;
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function formatBytes(value: unknown): string {
  const n = typeof value === "number" ? value : Number.NaN;
  if (!Number.isFinite(n) || n <= 0) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${Math.round(n / (1024 * 1024))} MB`;
}

function isIrreversibleCommand(cmd: string): boolean {
  const lower = cmd.toLowerCase();
  return (
    lower.includes("rm -rf") ||
    lower.startsWith("rm ") ||
    lower.includes("mkfs") ||
    lower.includes("dd if=")
  );
}

/** Heuristic: MCP tool names that look like writes/deletes/execs. */
function isMutatingMCPTool(name: string): boolean {
  const n = name.toLowerCase();
  return (
    n.includes("write") ||
    n.includes("delete") ||
    n.includes("remove") ||
    n.includes("create") ||
    n.includes("update") ||
    n.includes("patch") ||
    n.includes("put") ||
    n.includes("send") ||
    n.includes("post") ||
    n.includes("exec") ||
    n.includes("run") ||
    n.includes("destroy") ||
    n.includes("drop") ||
    n.includes("insert") ||
    n.includes("mutate")
  );
}

export function approvalCopy(
  capability: string,
  context?: Record<string, unknown>,
  constraints?: Record<string, unknown>,
): ApprovalCopy {
  const ctx = asRecord(context);
  const input = asRecord(ctx.input);

  if (capability === "filesystem.write") {
    const path = asString(input.path || ctx.path);
    const bytes = formatBytes(input.bytes || ctx.bytes);
    if (path && bytes) {
      return { summary: `Write ${bytes} to ${path}` };
    }
    if (path) {
      return { summary: `Write file content to ${path}` };
    }
    return { summary: "Write files in your workspace" };
  }

  if (capability === "filesystem.read") {
    const path = asString(input.path || ctx.path);
    if (path) {
      return { summary: `Read files from ${path}` };
    }
    return { summary: "Read files in your workspace" };
  }

  if (capability === "filesystem.list") {
    const path = asString(input.path || ctx.path);
    if (path) {
      return { summary: `List the contents of ${path}` };
    }
    return { summary: "List files in your workspace" };
  }

  if (capability === "filesystem.context") {
    const path = asString(input.path || ctx.path);
    if (path) {
      return { summary: `Attach the contents of ${path} as context` };
    }
    return { summary: "Attach folder contents as context for the run" };
  }

  if (capability === "llm.chat") {
    return { summary: "Send a prompt to the configured language model" };
  }

  if (capability === "command.exec") {
    const cmd = asString(input.command || ctx.command || ctx.input);
    if (cmd) {
      if (isIrreversibleCommand(cmd)) {
        return {
          summary: `Run command: ${cmd}. This action may permanently delete data.`,
          dangerSignal: "irreversible",
        };
      }
      return { summary: `Run command: ${cmd}`, dangerSignal: "shell" };
    }
    return { summary: "Run a shell command", dangerSignal: "shell" };
  }

  if (capability === "mcp.tools" || capability.startsWith("mcp.")) {
    const tool =
      asString(input.tool || ctx.tool) ||
      capability.split(".").slice(2).join(".") ||
      "";
    const mutate = isMutatingMCPTool(tool || capability);
    if (tool) {
      return {
        summary: `Call MCP tool ${tool}`,
        dangerSignal: mutate ? "irreversible" : "network",
      };
    }
    return {
      summary: "Call a tool on a connected MCP server",
      dangerSignal: "network",
    };
  }

  if (capability === "network.outgoing") {
    const host = asString(input.host || ctx.host || input.url || ctx.url);
    if (host) {
      return { summary: `Send a request to ${host}`, dangerSignal: "network" };
    }
    return { summary: "Send an outgoing network request", dangerSignal: "network" };
  }

  // Unknown capability fallback. We never tell the user to "ask a developer"
  // — that signals the product has failed them. Instead, derive a plain
  // sentence from the capability string itself ("network.outgoing" →
  // "perform a network.outgoing action"). The raw-details disclosure on
  // the approval card surfaces the structured context for power users.
  const constrained = constraints && Object.keys(constraints).length > 0;
  const humanized = humanizeCapability(capability);
  return {
    summary: constrained
      ? `Allow this assistant to ${humanized} with the listed constraints.`
      : `Allow this assistant to ${humanized}.`,
  };
}

function humanizeCapability(capability: string): string {
  // Per-tool MCP caps: "mcp.docs.write_file" → "call MCP tool write_file"
  const mcpParts = capability.split(".");
  if (mcpParts.length >= 3 && mcpParts[0] === "mcp") {
    const tool = mcpParts.slice(2).join(".");
    return `call MCP tool ${tool}`;
  }
  // "filesystem.write" → "perform a filesystem write"
  // Falls back to the raw capability string when it doesn't fit the
  // dotted-namespace convention so we never invent meaning we don't have.
  const parts = capability.split(".");
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return `perform the "${capability}" capability`;
  }
  const [namespace, action] = parts;
  return `${action} via ${namespace}`;
}
