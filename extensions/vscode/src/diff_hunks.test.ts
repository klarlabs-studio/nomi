import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  applySkippedHunks,
  listHunks,
  parseDiffStructure,
  rebuildDiff,
} from "./diff_hunks.js";

const SAMPLE = `--- a/foo.ts
+++ b/foo.ts
@@ -1,3 +1,3 @@
 keep
-old1
+new1
@@ -10,2 +10,2 @@
 context
-old2
+new2
--- a/bar.ts
+++ b/bar.ts
@@ -1 +1 @@
-only
+changed
`;

describe("parseDiffStructure", () => {
  it("splits files and hunks", () => {
    const blocks = parseDiffStructure(SAMPLE);
    assert.equal(blocks.length, 2);
    assert.equal(blocks[0]!.fileLabel, "foo.ts");
    assert.equal(blocks[0]!.hunks.length, 2);
    assert.equal(blocks[1]!.fileLabel, "bar.ts");
    assert.equal(blocks[1]!.hunks.length, 1);
  });
});

describe("rebuildDiff / applySkippedHunks", () => {
  it("drops skipped hunks and empty files", () => {
    const blocks = parseDiffStructure(SAMPLE);
    const skipped = new Set(["foo.ts#1", "bar.ts#0"]);
    const out = rebuildDiff(blocks, skipped);
    assert.match(out, /foo\.ts/);
    assert.match(out, /\+new1/);
    assert.doesNotMatch(out, /\+new2/);
    assert.doesNotMatch(out, /bar\.ts/);
  });

  it("applySkippedHunks is parse+rebuild", () => {
    const out = applySkippedHunks(SAMPLE, new Set(["foo.ts#0"]));
    assert.match(out, /\+new2/);
    assert.doesNotMatch(out, /\+new1/);
  });
});

describe("listHunks", () => {
  it("flattens keys for the UI", () => {
    const items = listHunks(SAMPLE);
    assert.deepEqual(
      items.map((i) => i.key),
      ["foo.ts#0", "foo.ts#1", "bar.ts#0"],
    );
    assert.equal(items[0]!.added, 1);
    assert.equal(items[0]!.removed, 1);
  });
});
