import { describe, expect, it } from "vitest";

import { highlightLines, normalizeLang, langFromPath } from "@/lib/highlighter";

describe("highlighter", () => {
  it("normalises common language aliases", () => {
    expect(normalizeLang("TS")).toBe("typescript");
    expect(normalizeLang("javascript")).toBe("javascript");
    expect(normalizeLang("golang")).toBe("go");
    expect(normalizeLang("rs")).toBe("rust");
    expect(normalizeLang("yml")).toBe("yaml");
  });

  it("returns null for unbundled languages", () => {
    expect(normalizeLang("brainfuck")).toBe(null);
    expect(normalizeLang(undefined)).toBe(null);
    expect(normalizeLang("")).toBe(null);
  });

  it("sniffs language from file extension", () => {
    expect(langFromPath("src/api/server.go")).toBe("go");
    expect(langFromPath("App.tsx")).toBe("tsx");
    expect(langFromPath("noext")).toBe(null);
  });

  it("highlightLines returns null for null lang without touching Shiki", async () => {
    const out = await highlightLines("foo := 1\nbar := 2", null);
    expect(out).toBe(null);
  });

  // Note: the real Shiki path is exercised by app/e2e — a unit test
  // here would need to mock the dynamic import of `shiki` which is
  // brittle. The fallback paths (null lang, Shiki init failure) are
  // the ones we actually want to lock down at this layer.
});
