import { describe, expect, it } from "vitest";
import {
  MCP_SERVER_PRESETS,
  applyMcpPresetConfig,
  findMcpPreset,
} from "@/lib/mcp-presets";

describe("mcp-presets", () => {
  it("includes a custom blank preset and at least one ready-to-create server", () => {
    expect(findMcpPreset("custom")).toBeDefined();
    expect(MCP_SERVER_PRESETS.some((p) => p.readyToCreate)).toBe(true);
  });

  it("applyMcpPresetConfig fills stdio fields and clears endpoint", () => {
    const preset = findMcpPreset("memory")!;
    const next = applyMcpPresetConfig(preset, {
      transport: "http",
      endpoint: "https://old.example",
      command: "old",
      args: "old-args",
    });
    expect(next.transport).toBe("stdio");
    expect(next.command).toBe("npx");
    expect(next.args).toContain("@modelcontextprotocol/server-memory");
    expect(next.endpoint).toBe("");
  });

  it("applyMcpPresetConfig fills http endpoint and clears stdio fields", () => {
    const preset = findMcpPreset("http-remote")!;
    const next = applyMcpPresetConfig(preset, {
      transport: "stdio",
      command: "npx",
      args: "-y something",
      endpoint: "",
    });
    expect(next.transport).toBe("http");
    expect(next.endpoint).toMatch(/^https?:\/\//);
    expect(next.command).toBe("");
    expect(next.args).toBe("");
  });

  it("path-scoped presets are not readyToCreate without edits", () => {
    expect(findMcpPreset("filesystem")!.readyToCreate).toBe(false);
    expect(findMcpPreset("git")!.readyToCreate).toBe(false);
    expect(findMcpPreset("memory")!.readyToCreate).toBe(true);
  });
});
