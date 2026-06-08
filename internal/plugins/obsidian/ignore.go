package obsidian

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ignoreMatcher applies a small subset of gitignore-style patterns
// loaded from `.obsidianignore` at the vault root. Supported syntax:
//
//   - `Templates/`               directory match (anywhere)
//   - `*.tmp`                    glob extension match (anywhere)
//   - `private/**`               recursive prefix match
//   - `private/notes.md`         exact relative path
//   - `# comment` and blanks     ignored
//
// Out of scope (deferred until anyone asks): negation (`!pattern`),
// character classes, and the full gitignore precedence rules. We
// match by pattern in declaration order; first hit wins. Anything
// more elaborate than the patterns above is treated as an exact-
// path or basename match per filepath.Match's behavior.
type ignoreMatcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	raw     string
	dirOnly bool
	// Compiled forms — exactly one is set per rule.
	exactPath string
	prefix    string // for `prefix/**`
	basename  string // for `name` or `name/`
	globExt   string // for `*.ext`
}

func loadObsidianIgnore(vaultPath string) *ignoreMatcher {
	m := &ignoreMatcher{}
	f, err := os.Open(filepath.Join(vaultPath, ".obsidianignore"))
	if err != nil {
		return m
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rule, ok := compileIgnoreRule(line); ok {
			m.rules = append(m.rules, rule)
		}
	}
	return m
}

func compileIgnoreRule(raw string) (ignoreRule, bool) {
	rule := ignoreRule{raw: raw}
	pat := strings.TrimPrefix(raw, "/")
	if strings.HasSuffix(pat, "/") {
		rule.dirOnly = true
		pat = strings.TrimSuffix(pat, "/")
	}
	if pat == "" {
		return rule, false
	}

	switch {
	case strings.HasSuffix(pat, "/**"):
		rule.prefix = strings.TrimSuffix(pat, "/**")
	case strings.HasPrefix(pat, "*."):
		rule.globExt = strings.TrimPrefix(pat, "*")
	case strings.ContainsAny(pat, "/*?"):
		rule.exactPath = pat
	default:
		rule.basename = pat
	}
	return rule, true
}

func (m *ignoreMatcher) matchesFile(rel string) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	for _, r := range m.rules {
		if r.dirOnly {
			continue
		}
		if r.matchesFile(rel, base) {
			return true
		}
	}
	return false
}

func (m *ignoreMatcher) matchesDir(rel string) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	for _, r := range m.rules {
		if r.matchesDir(rel, base) {
			return true
		}
	}
	return false
}

func (r ignoreRule) matchesFile(rel, base string) bool {
	switch {
	case r.exactPath != "":
		if rel == r.exactPath {
			return true
		}
		matched, _ := filepath.Match(r.exactPath, rel)
		return matched
	case r.prefix != "":
		return rel == r.prefix || strings.HasPrefix(rel, r.prefix+"/")
	case r.basename != "":
		return base == r.basename
	case r.globExt != "":
		return strings.HasSuffix(strings.ToLower(base), strings.ToLower(r.globExt))
	}
	return false
}

func (r ignoreRule) matchesDir(rel, base string) bool {
	switch {
	case r.exactPath != "":
		// Directory rules with explicit slashes like "private/notes" match
		// the directory if rel is a prefix of the pattern or equal.
		return rel == r.exactPath
	case r.prefix != "":
		return rel == r.prefix || strings.HasPrefix(rel, r.prefix+"/")
	case r.basename != "":
		return base == r.basename
	}
	return false
}
