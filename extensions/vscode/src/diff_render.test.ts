import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { DIFF_PREVIEW_CSS, renderDiffPreviewHtml } from "./diff_render";

const SAMPLE = `--- a/hello.go
+++ b/hello.go
@@ -1,3 +1,4 @@
 package main
 
-func hi() {}
+func hi() {
+	fmt.Println("hi")
+}
`;

describe("diff_render", () => {
  it("exports DiffPreview CSS with split toggle hooks", () => {
    assert.match(DIFF_PREVIEW_CSS, /body\.diff-split/);
    assert.match(DIFF_PREVIEW_CSS, /\.view-unified/);
    assert.match(DIFF_PREVIEW_CSS, /\.view-split/);
  });

  it("renders unified + split markup with Shiki tokens for Go", async () => {
    const html = await renderDiffPreviewHtml(SAMPLE, true);
    assert.match(html, /class="diff-preview"/);
    assert.match(html, /view-unified/);
    assert.match(html, /view-split/);
    assert.match(html, /hello\.go/);
    assert.match(html, /diff-line add/);
    assert.match(html, /diff-line rem/);
    // Shiki injects styled spans for Go keywords when the grammar loads.
    assert.match(html, /<span style=/);
  });

  it("renders clickable file labels for openPath", async () => {
    const html = await renderDiffPreviewHtml(SAMPLE, false);
    assert.match(html, /data-open-path="hello\.go"/);
    assert.match(html, /class="file-label"/);
    assert.match(html, /class="file-chip"/);
  });
});
