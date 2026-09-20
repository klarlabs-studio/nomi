package runtime

import (
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestSanitizeEditorContext_TruncatesAndDropsSecrets(t *testing.T) {
	huge := strings.Repeat("x", maxSelectionBytes+100)
	in := &domain.EditorContext{
		Source:           "vscode",
		WorkspaceFolders: []string{"/proj", "/proj/.ssh/config", "  "},
		OpenTabs:         []string{"src/a.ts", ".env", "src/b.ts"},
		Active: &domain.EditorActiveFile{
			Path:       "src/a.ts",
			LanguageID: "typescript",
			Selection:  &domain.EditorSelection{StartLine: 1, EndLine: 2, Text: huge},
		},
	}
	out := SanitizeEditorContext(in)
	if out == nil {
		t.Fatal("expected sanitized context")
	}
	if len(out.WorkspaceFolders) != 1 || out.WorkspaceFolders[0] != "/proj" {
		t.Fatalf("folders=%v", out.WorkspaceFolders)
	}
	if len(out.OpenTabs) != 2 {
		t.Fatalf("tabs=%v", out.OpenTabs)
	}
	for _, tab := range out.OpenTabs {
		if tab == ".env" {
			t.Fatal(".env should be dropped")
		}
	}
	if out.Active == nil || out.Active.Selection == nil {
		t.Fatal("expected active selection")
	}
	if len(out.Active.Selection.Text) > maxSelectionBytes+20 {
		t.Fatalf("selection not truncated: %d", len(out.Active.Selection.Text))
	}
}

func TestFormatEditorContext_IncludesSelection(t *testing.T) {
	ec := SanitizeEditorContext(&domain.EditorContext{
		Source: "vscode",
		Active: &domain.EditorActiveFile{
			Path: "main.go",
			Selection: &domain.EditorSelection{
				StartLine: 10,
				EndLine:   12,
				Text:      "func main() {}\n",
			},
		},
	})
	s := FormatEditorContext(ec)
	for _, needle := range []string{"source: vscode", "active_file: main.go", "selection_lines: 10-12", "func main()"} {
		if !strings.Contains(s, needle) {
			t.Fatalf("missing %q in:\n%s", needle, s)
		}
	}
}

func TestSanitizeEditorContext_Empty(t *testing.T) {
	if SanitizeEditorContext(nil) != nil {
		t.Fatal("nil in → nil out")
	}
	if SanitizeEditorContext(&domain.EditorContext{OpenTabs: []string{".env"}}) != nil {
		t.Fatal("only secrets → nil")
	}
}
