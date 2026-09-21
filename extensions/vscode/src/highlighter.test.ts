import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { langFromPath, normalizeLang, highlightLines } from "./highlighter";

describe("highlighter", () => {
  it("normalises common language aliases", () => {
    assert.equal(normalizeLang("TS"), "typescript");
    assert.equal(normalizeLang("javascript"), "javascript");
    assert.equal(normalizeLang("golang"), "go");
    assert.equal(normalizeLang("rs"), "rust");
    assert.equal(normalizeLang("yml"), "yaml");
  });

  it("returns null for unbundled languages", () => {
    assert.equal(normalizeLang("brainfuck"), null);
    assert.equal(normalizeLang(undefined), null);
    assert.equal(normalizeLang(""), null);
  });

  it("sniffs language from file extension", () => {
    assert.equal(langFromPath("src/api/server.go"), "go");
    assert.equal(langFromPath("App.tsx"), "tsx");
    assert.equal(langFromPath("noext"), null);
  });

  it("highlightLines returns null for null lang without touching Shiki", async () => {
    const out = await highlightLines("foo := 1\nbar := 2", null, false);
    assert.equal(out, null);
  });
});
