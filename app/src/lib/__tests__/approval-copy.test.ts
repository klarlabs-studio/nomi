import { describe, expect, it } from "vitest";

import { approvalCopy } from "@/lib/approval-copy";

describe("approvalCopy", () => {
  it("renders command approvals with shell danger", () => {
    const copy = approvalCopy("command.exec", { input: "git status" });
    expect(copy.summary).toContain("Run command: git status");
    expect(copy.dangerSignal).toBe("shell");
  });

  it("flags irreversible delete commands", () => {
    const copy = approvalCopy("command.exec", { input: "rm -rf /tmp/junk" });
    expect(copy.dangerSignal).toBe("irreversible");
  });

  it("summarizes filesystem write with byte size", () => {
    const copy = approvalCopy("filesystem.write", {
      input: { path: "/tmp/readme.md", bytes: 2048 },
    });
    expect(copy.summary).toContain("Write 2 KB to /tmp/readme.md");
  });

  it("falls back to a plain-English sentence for unknown capabilities", () => {
    // Never tell the user to "ask a developer" — that's a product-failure
    // signal. The fallback should still be readable by a non-developer.
    const copy = approvalCopy("network.something", {});
    expect(copy.summary).not.toContain("Ask a developer");
    expect(copy.summary).toContain("something");
    expect(copy.summary.toLowerCase()).toContain("allow this assistant");
  });

  it("falls back without splitting non-dotted capability strings", () => {
    const copy = approvalCopy("weirdcap", {});
    expect(copy.summary).not.toContain("Ask a developer");
    expect(copy.summary).toContain("weirdcap");
  });

  it("renders plain-English copy for filesystem.list with a path", () => {
    const copy = approvalCopy("filesystem.list", { input: { path: "/tmp" } });
    expect(copy.summary).toContain("/tmp");
  });

  it("renders plain-English copy for llm.chat", () => {
    const copy = approvalCopy("llm.chat", {});
    expect(copy.summary.toLowerCase()).toContain("language model");
  });

  it("renders MCP tool approvals with the tool name", () => {
    const copy = approvalCopy("mcp.tools", { input: { tool: "search" } });
    expect(copy.summary).toContain("search");
    expect(copy.dangerSignal).toBe("network");
  });

  it("uses per-tool MCP capabilities in copy", () => {
    const copy = approvalCopy("mcp.docs.search", {});
    expect(copy.summary).toContain("search");
    expect(copy.dangerSignal).toBe("network");
  });

  it("flags mutate-shaped MCP tools as irreversible", () => {
    const copy = approvalCopy("mcp.docs.write_file", {});
    expect(copy.summary).toContain("write_file");
    expect(copy.dangerSignal).toBe("irreversible");
  });
});
