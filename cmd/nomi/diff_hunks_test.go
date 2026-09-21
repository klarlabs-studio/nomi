package main

import (
	"strings"
	"testing"
)

const sampleDiff = `--- a/foo.ts
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
`

func TestParseDiffStructure(t *testing.T) {
	blocks := parseDiffStructure(sampleDiff)
	if len(blocks) != 2 {
		t.Fatalf("files = %d, want 2", len(blocks))
	}
	if blocks[0].fileLabel != "foo.ts" || len(blocks[0].hunks) != 2 {
		t.Fatalf("foo: label=%s hunks=%d", blocks[0].fileLabel, len(blocks[0].hunks))
	}
	if blocks[1].fileLabel != "bar.ts" || len(blocks[1].hunks) != 1 {
		t.Fatalf("bar: label=%s hunks=%d", blocks[1].fileLabel, len(blocks[1].hunks))
	}
}

func TestApplySkippedHunks(t *testing.T) {
	out := applySkippedHunks(sampleDiff, map[string]bool{"foo.ts#1": true, "bar.ts#0": true})
	if !strings.Contains(out, "foo.ts") || !strings.Contains(out, "+new1") {
		t.Fatalf("kept hunk missing: %s", out)
	}
	if strings.Contains(out, "+new2") || strings.Contains(out, "bar.ts") {
		t.Fatalf("skipped content still present: %s", out)
	}
}

func TestListHunks(t *testing.T) {
	items := listHunks(sampleDiff)
	if len(items) != 3 {
		t.Fatalf("len = %d", len(items))
	}
	want := []string{"foo.ts#0", "foo.ts#1", "bar.ts#0"}
	for i, k := range want {
		if items[i].key != k {
			t.Fatalf("key[%d] = %s, want %s", i, items[i].key, k)
		}
	}
}

func TestApplyHunkSkipsToPlan(t *testing.T) {
	plan := &planPayload{Steps: []planStep{{
		ID: "s1", ExpectedTool: "filesystem.patch",
		Arguments: map[string]any{"diff": sampleDiff},
	}}}
	changed := applyHunkSkipsToPlan(plan, map[string][]string{
		"s1": {"foo.ts#1"},
	})
	if !changed {
		t.Fatal("expected change")
	}
	diff, _ := plan.Steps[0].Arguments["diff"].(string)
	if !strings.Contains(diff, "+new1") || strings.Contains(diff, "+new2") {
		t.Fatalf("diff not rebuilt: %s", diff)
	}
}
