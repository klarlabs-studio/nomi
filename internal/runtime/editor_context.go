package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.klarlabs.de/nomi/internal/domain"
)

// Budgets keep editor context from crowding out folder/plugin blocks
// under the planner's 16 KiB total cap.
const (
	maxEditorContextBytes = 8 * 1024
	maxSelectionBytes     = 4 * 1024
	maxOpenTabs           = 20
	maxWorkspaceFolders   = 5
	maxPathChars          = 512
)

// SanitizeEditorContext copies and truncates a client-supplied editor
// context at the API boundary. Returns nil when nothing usable remains.
func SanitizeEditorContext(in *domain.EditorContext) *domain.EditorContext {
	if in == nil {
		return nil
	}
	out := &domain.EditorContext{
		Source: strings.TrimSpace(in.Source),
	}
	if out.Source == "" {
		out.Source = "editor"
	}
	for _, f := range in.WorkspaceFolders {
		f = truncatePath(f)
		if f == "" || looksSecretPath(f) {
			continue
		}
		out.WorkspaceFolders = append(out.WorkspaceFolders, f)
		if len(out.WorkspaceFolders) >= maxWorkspaceFolders {
			break
		}
	}
	for _, t := range in.OpenTabs {
		t = truncatePath(t)
		if t == "" || looksSecretPath(t) {
			continue
		}
		out.OpenTabs = append(out.OpenTabs, t)
		if len(out.OpenTabs) >= maxOpenTabs {
			break
		}
	}
	if in.Active != nil && in.Active.Path != "" {
		p := truncatePath(in.Active.Path)
		if p != "" && !looksSecretPath(p) {
			active := &domain.EditorActiveFile{
				Path:       p,
				LanguageID: strings.TrimSpace(in.Active.LanguageID),
			}
			if sel := in.Active.Selection; sel != nil && strings.TrimSpace(sel.Text) != "" {
				text := truncateRunes(sel.Text, maxSelectionBytes)
				active.Selection = &domain.EditorSelection{
					StartLine: sel.StartLine,
					EndLine:   sel.EndLine,
					Text:      text,
				}
			}
			out.Active = active
		}
	}
	if len(out.WorkspaceFolders) == 0 && len(out.OpenTabs) == 0 && out.Active == nil {
		return nil
	}
	return out
}

// FormatEditorContext renders a sanitized editor context as a plain-text
// block for the planner prompt (caller wraps with trusted="false").
func FormatEditorContext(ec *domain.EditorContext) string {
	if ec == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", ec.Source)
	if len(ec.WorkspaceFolders) > 0 {
		b.WriteString("workspace_folders:\n")
		for _, f := range ec.WorkspaceFolders {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if len(ec.OpenTabs) > 0 {
		b.WriteString("open_tabs:\n")
		for _, t := range ec.OpenTabs {
			fmt.Fprintf(&b, "  - %s\n", t)
		}
	}
	if ec.Active != nil {
		fmt.Fprintf(&b, "active_file: %s\n", ec.Active.Path)
		if ec.Active.LanguageID != "" {
			fmt.Fprintf(&b, "language: %s\n", ec.Active.LanguageID)
		}
		if sel := ec.Active.Selection; sel != nil {
			fmt.Fprintf(&b, "selection_lines: %d-%d\n", sel.StartLine, sel.EndLine)
			b.WriteString("selection:\n")
			b.WriteString(sel.Text)
			if !strings.HasSuffix(sel.Text, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	s := b.String()
	if len(s) > maxEditorContextBytes {
		s = s[:maxEditorContextBytes] + "\n…[truncated]\n"
	}
	return s
}

func truncatePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if utf8.RuneCountInString(p) > maxPathChars {
		rs := []rune(p)
		p = string(rs[:maxPathChars])
	}
	return p
}

func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Cut on a rune boundary near maxBytes.
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes] + "\n…[truncated]"
}

func looksSecretPath(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	full := strings.ToLower(filepath.ToSlash(p))
	secretNames := []string{
		".env", ".env.local", ".env.production", ".env.development",
		"credentials", "credentials.json", ".npmrc", ".pypirc",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"auth.token", "api.endpoint",
	}
	for _, n := range secretNames {
		if base == n || strings.HasPrefix(base, n+".") {
			return true
		}
	}
	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".p12") {
		return true
	}
	if strings.Contains(full, "/.aws/") || strings.Contains(full, "/.ssh/") ||
		strings.HasSuffix(full, "/.aws") || strings.HasSuffix(full, "/.ssh") ||
		base == ".ssh" || base == ".aws" || base == ".gnupg" {
		return true
	}
	if strings.Contains(base, "secret") || strings.Contains(base, "passwd") || strings.Contains(base, "password") {
		return true
	}
	return false
}
