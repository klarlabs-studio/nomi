import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  buildEditorContext,
  isSecretPath,
  truncateSelection,
} from "./editor_context";

describe("isSecretPath", () => {
  it("flags env and key material", () => {
    assert.equal(isSecretPath("/proj/.env"), true);
    assert.equal(isSecretPath("/proj/.env.local"), true);
    assert.equal(isSecretPath("/home/me/.ssh/id_rsa"), true);
    assert.equal(isSecretPath("certs/server.pem"), true);
    assert.equal(isSecretPath("src/app.ts"), false);
  });
});

describe("buildEditorContext", () => {
  it("drops secrets and caps lists", () => {
    const ctx = buildEditorContext({
      workspaceFolders: ["/proj", "/proj/.ssh"],
      openTabs: ["a.ts", ".env", "b.ts"],
      active: {
        path: "a.ts",
        languageId: "typescript",
        selectionText: "const x = 1;",
        startLine: 3,
        endLine: 3,
      },
    });
    assert.ok(ctx);
    assert.deepEqual(ctx!.workspace_folders, ["/proj"]);
    assert.deepEqual(ctx!.open_tabs, ["a.ts", "b.ts"]);
    assert.equal(ctx!.active?.path, "a.ts");
    assert.equal(ctx!.active?.selection?.text, "const x = 1;");
  });

  it("returns undefined when everything is filtered", () => {
    assert.equal(
      buildEditorContext({ workspaceFolders: [], openTabs: [".env"] }),
      undefined,
    );
  });
});

describe("truncateSelection", () => {
  it("appends marker past the limit", () => {
    const s = truncateSelection("x".repeat(10), 5);
    assert.ok(s.startsWith("xxxxx"));
    assert.ok(s.includes("[truncated]"));
  });
});
