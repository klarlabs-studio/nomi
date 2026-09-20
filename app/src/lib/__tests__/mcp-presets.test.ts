import { describe, expect, it } from "vitest";
import {
  MCP_EMPTY_STATE_PRESET_IDS,
  MCP_SERVER_PRESETS,
  applyMcpPresetConfig,
  filterMcpPresets,
  findMcpPreset,
  mcpConfigHasPlaceholder,
  mcpPresetCreateBlockedReason,
  parseEnvLiteralLines,
} from "@/lib/mcp-presets";

describe("mcp-presets", () => {
  it("includes a custom blank preset and at least one ready-to-create server", () => {
    expect(findMcpPreset("custom")).toBeDefined();
    expect(MCP_SERVER_PRESETS.some((p) => p.readyToCreate)).toBe(true);
  });

  it("every catalog preset has category, runtime, docs, and description", () => {
    for (const p of MCP_SERVER_PRESETS) {
      expect(p.category, p.id).toBeTruthy();
      expect(p.runtime, p.id).toBeTruthy();
      expect(p.description.trim().length, p.id).toBeGreaterThan(0);
      expect(p.setupNote.trim().length, p.id).toBeGreaterThan(0);
      expect(p.docURL, p.id).toMatch(/^https?:\/\//);
    }
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

  it("includes github and postgres env-secret presets", () => {
    const gh = findMcpPreset("github")!;
    expect(gh.envCredentials?.[0]?.key).toBe("GITHUB_PERSONAL_ACCESS_TOKEN");
    expect(gh.category).toBe("cloud");
    expect(gh.runtime).toBe("docker");
    const pg = findMcpPreset("postgres")!;
    expect(pg.args).toContain("${DATABASE_URL}");
    expect(pg.envCredentials?.[0]?.key).toBe("DATABASE_URL");
    expect(pg.category).toBe("data");
  });

  it("parseEnvLiteralLines skips blanks and comments", () => {
    const got = parseEnvLiteralLines("A=1\n# skip\nB = two\n");
    expect(got).toEqual({ A: "1", B: "two" });
  });

  it("filterMcpPresets searches label/description and category", () => {
    const mem = filterMcpPresets("memory");
    expect(mem.map((p) => p.id)).toContain("memory");
    expect(mem.every((p) => p.id !== "custom")).toBe(true);

    const cloud = filterMcpPresets("", "cloud");
    expect(cloud.map((p) => p.id)).toEqual(["github"]);

    const docker = filterMcpPresets("docker");
    expect(docker.some((p) => p.id === "github")).toBe(true);
  });

  it("mcpConfigHasPlaceholder catches path and example endpoint tokens", () => {
    expect(
      mcpConfigHasPlaceholder({
        command: "npx",
        args: "-y @modelcontextprotocol/server-filesystem /path/to/allowed",
      }),
    ).toBe(true);
    expect(
      mcpConfigHasPlaceholder({
        endpoint: "https://mcp.example.com/sse",
      }),
    ).toBe(true);
    expect(
      mcpConfigHasPlaceholder({
        command: "npx",
        args: "-y @modelcontextprotocol/server-memory",
      }),
    ).toBe(false);
  });

  it("mcpPresetCreateBlockedReason guards secrets and placeholders", () => {
    const fs = findMcpPreset("filesystem")!;
    expect(
      mcpPresetCreateBlockedReason(
        fs,
        applyMcpPresetConfig(fs, {}),
        {},
      ),
    ).toMatch(/placeholder/i);

    const edited = applyMcpPresetConfig(fs, {});
    edited.args = "-y @modelcontextprotocol/server-filesystem /Users/me/proj";
    expect(mcpPresetCreateBlockedReason(fs, edited, {})).toBeNull();

    const gh = findMcpPreset("github")!;
    expect(
      mcpPresetCreateBlockedReason(gh, applyMcpPresetConfig(gh, {}), {}),
    ).toMatch(/token/i);
    expect(
      mcpPresetCreateBlockedReason(gh, applyMcpPresetConfig(gh, {}), {
        GITHUB_PERSONAL_ACCESS_TOKEN: "ghp_x",
      }),
    ).toBeNull();

    const mem = findMcpPreset("memory")!;
    expect(
      mcpPresetCreateBlockedReason(mem, applyMcpPresetConfig(mem, {}), {}),
    ).toBeNull();
  });

  it("empty-state preset ids resolve", () => {
    for (const id of MCP_EMPTY_STATE_PRESET_IDS) {
      expect(findMcpPreset(id)).toBeDefined();
    }
  });
});
