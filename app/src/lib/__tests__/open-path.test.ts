import { describe, expect, it } from "vitest";
import { resolveOpenPathFs } from "@/lib/open-path";

describe("resolveOpenPathFs", () => {
  it("rejects empty and /dev/null", () => {
    expect(resolveOpenPathFs("")).toBeNull();
    expect(resolveOpenPathFs("/dev/null")).toBeNull();
  });

  it("keeps absolute unix and windows paths", () => {
    expect(resolveOpenPathFs("/tmp/x.go")).toBe("/tmp/x.go");
    expect(resolveOpenPathFs("C:\\Users\\a\\b.ts")).toBe("C:\\Users\\a\\b.ts");
  });

  it("joins relative paths to workspace root", () => {
    expect(resolveOpenPathFs("src/main.go", "/proj")).toBe("/proj/src/main.go");
    expect(resolveOpenPathFs("./foo.ts", "/proj/")).toBe("/proj/foo.ts");
  });

  it("returns relative as-is without a workspace root", () => {
    expect(resolveOpenPathFs("hello.go")).toBe("hello.go");
  });
});
