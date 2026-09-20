import { describe, expect, it } from "vitest";
import {
  MCP_SERVER_PRESETS,
  applyMcpPresetConfig,
  findMcpPreset,
  parseEnvLiteralLines,
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

  it("includes github and postgres env-secret presets", () => {
    const gh = findMcpPreset("github")!;
    expect(gh.envCredentials?.[0]?.key).toBe("GITHUB_PERSONAL_ACCESS_TOKEN");
    const pg = findMcpPreset("postgres")!;
    expect(pg.args).toContain("${DATABASE_URL}");
    expect(pg.envCredentials?.[0]?.key).toBe("DATABASE_URL");
  });

  it("parseEnvLiteralLines skips blanks and comments", () => {
    const got = parseEnvLiteralLines("A=1\n# skip\nB = two\n");
    expect(got).toEqual({ A: "1", B: "two" });
  });
});
