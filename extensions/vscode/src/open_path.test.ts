import assert from "node:assert/strict";
import path from "node:path";
import { describe, it } from "node:test";
import { resolveOpenPathFs } from "./open_path_fs";

describe("resolveOpenPathFs", () => {
  it("rejects empty and /dev/null", () => {
    assert.equal(resolveOpenPathFs("", "/proj"), null);
    assert.equal(resolveOpenPathFs("/dev/null", "/proj"), null);
  });

  it("joins relative paths to the workspace root", () => {
    const got = resolveOpenPathFs("src/main.go", "/proj");
    assert.equal(got, path.join("/proj", "src/main.go"));
  });

  it("keeps absolute paths", () => {
    assert.equal(resolveOpenPathFs("/tmp/x.go", "/proj"), "/tmp/x.go");
  });
});
