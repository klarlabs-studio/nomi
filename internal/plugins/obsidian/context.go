package obsidian

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

const (
	contextSourceName      = "obsidian.vault"
	contextMaxNotes        = 5
	contextMaxLinkedNotes  = 5
	contextSnippetMaxBytes = 600
	contextOutputMaxBytes  = 4000
	contextMinTokenLen     = 3
	contextFollowLinkDepth = 1
)

// ContextSources implements plugins.ContextSourceProvider. Returns one
// vaultContextSource per enabled Obsidian connection so the runtime
// can pair each ContextSource with the right (assistant, connection)
// binding at plan time.
//
// Disabled or misconfigured connections are skipped silently — a
// vault without a path can't index anything, and surfacing a partial
// source would confuse the planner. Connection-level errors (missing
// vault folder on disk) are deferred to Query() so the failure mode
// is the same as a transient I/O hiccup.
func (p *Plugin) ContextSources() []plugins.ContextSource {
	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		log.Printf("obsidian: list connections for context sources: %v", err)
		return nil
	}
	var out []plugins.ContextSource
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		root, err := VaultPathForConnection(conn)
		if err != nil {
			log.Printf("obsidian: skip context source for %s: %v", conn.ID, err)
			continue
		}
		out = append(out, &vaultContextSource{
			connectionID: conn.ID,
			vaultPath:    root,
			displayName:  displayName(conn),
		})
	}
	return out
}

func displayName(conn *domain.Connection) string {
	if conn == nil {
		return ""
	}
	if name, ok := conn.Config[configDisplayName].(string); ok && name != "" {
		return name
	}
	return conn.Name
}

// vaultContextSource is the per-Connection ContextSource implementation.
// Holds no state beyond identity — the index is rebuilt on every Query
// because vault folders are local and walking a few hundred markdown
// files is comparable to the planner's own latency. If profiling shows
// this becomes a hot path, add an mtime-keyed cache; the API stays the
// same.
type vaultContextSource struct {
	connectionID string
	vaultPath    string
	displayName  string
}

func (v *vaultContextSource) ConnectionID() string { return v.connectionID }
func (v *vaultContextSource) Name() string         { return contextSourceName }

// Query walks the vault, scores every non-ignored note against the
// goal, formats the top hits plus their 1-hop neighbors, and returns
// the result as a markdown blob the planner can splice into its
// system prompt. Errors are surfaced (not swallowed) so the planner
// sees them in its own retry/backoff loop.
func (v *vaultContextSource) Query(_ context.Context, request plugins.ContextQueryRequest) (string, error) {
	if v.vaultPath == "" {
		return "", fmt.Errorf("obsidian.vault: vault path is empty")
	}
	if _, err := os.Stat(v.vaultPath); err != nil {
		return "", fmt.Errorf("obsidian.vault: vault inaccessible: %w", err)
	}

	ignore := loadObsidianIgnore(v.vaultPath)
	tokens := tokenizeGoal(request.Goal)

	notes, err := indexVault(v.vaultPath, ignore)
	if err != nil {
		return "", fmt.Errorf("obsidian.vault: index: %w", err)
	}

	scored := scoreNotes(notes, tokens)
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].modUnix > scored[j].modUnix
	})

	primary := pickPrimary(scored, contextMaxNotes)
	linked := pickLinked(notes, primary, contextMaxLinkedNotes)

	return renderContext(v.displayName, v.vaultPath, primary, linked), nil
}

// indexedNote is the in-memory snapshot the indexer produces for one
// markdown file. Body is the post-frontmatter content; basename is the
// filename without `.md`, used both for display and as the implicit
// wikilink target other notes use to point at this one.
type indexedNote struct {
	relPath  string
	basename string
	body     string
	tags     []string
	links    []string
	modUnix  int64

	score int
}

func indexVault(root string, ignore *ignoreMatcher) ([]indexedNote, error) {
	var out []indexedNote
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			if ignore.matchesDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		if ignore.matchesFile(rel) {
			return nil
		}

		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		fm, body := parseFrontmatter(string(raw))
		modUnix := int64(0)
		if info, err := d.Info(); err == nil {
			modUnix = info.ModTime().Unix()
		}
		out = append(out, indexedNote{
			relPath:  rel,
			basename: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())),
			body:     body,
			tags:     tagsFromFrontmatter(fm),
			links:    extractWikilinks(body),
			modUnix:  modUnix,
		})
		return nil
	})
	return out, err
}

func scoreNotes(notes []indexedNote, tokens []string) []indexedNote {
	if len(tokens) == 0 {
		return notes
	}
	out := make([]indexedNote, len(notes))
	for i, n := range notes {
		lowerBody := strings.ToLower(n.body)
		lowerName := strings.ToLower(n.basename)
		score := 0
		for _, tok := range tokens {
			score += 5 * countSubstring(lowerName, tok)
			for _, tag := range n.tags {
				if strings.Contains(strings.ToLower(tag), tok) {
					score += 3
				}
			}
			score += countSubstring(lowerBody, tok)
		}
		n.score = score
		out[i] = n
	}
	return out
}

func countSubstring(s, sub string) int {
	if sub == "" {
		return 0
	}
	return strings.Count(s, sub)
}

func pickPrimary(scored []indexedNote, max int) []indexedNote {
	out := make([]indexedNote, 0, max)
	for _, n := range scored {
		if n.score <= 0 {
			break
		}
		out = append(out, n)
		if len(out) >= max {
			break
		}
	}
	return out
}

// pickLinked walks the wikilinks of every primary hit one hop deep
// and returns up to max neighbor notes that aren't already in the
// primary set. The link graph helps the planner see "the note about
// X mentions Y" without the user having to surface Y manually.
func pickLinked(notes []indexedNote, primary []indexedNote, max int) []indexedNote {
	if len(primary) == 0 || max <= 0 {
		return nil
	}
	byBasename := map[string]*indexedNote{}
	for i := range notes {
		byBasename[strings.ToLower(notes[i].basename)] = &notes[i]
	}
	primarySet := map[string]bool{}
	for _, n := range primary {
		primarySet[strings.ToLower(n.basename)] = true
	}

	var out []indexedNote
	seen := map[string]bool{}
	for _, n := range primary {
		for _, raw := range n.links {
			target := strings.ToLower(strings.TrimSuffix(raw, ".md"))
			if idx := strings.LastIndex(target, "/"); idx >= 0 {
				target = target[idx+1:]
			}
			if primarySet[target] || seen[target] {
				continue
			}
			seen[target] = true
			if neighbor, ok := byBasename[target]; ok {
				out = append(out, *neighbor)
				if len(out) >= max {
					return out
				}
			}
		}
	}
	return out
}

func renderContext(displayName, vaultPath string, primary, linked []indexedNote) string {
	var b strings.Builder
	header := vaultPath
	if displayName != "" {
		header = fmt.Sprintf("%s (%s)", displayName, vaultPath)
	}
	fmt.Fprintf(&b, "# Vault context — %s\n\n", header)

	if len(primary) == 0 {
		b.WriteString("(no notes matched the current goal)\n")
		return capOutput(b.String())
	}

	b.WriteString("## Top matches\n\n")
	for _, n := range primary {
		writeNoteEntry(&b, n)
	}

	if len(linked) > 0 {
		b.WriteString("## Linked notes (1-hop)\n\n")
		for _, n := range linked {
			writeNoteEntry(&b, n)
		}
	}

	return capOutput(b.String())
}

func writeNoteEntry(b *strings.Builder, n indexedNote) {
	fmt.Fprintf(b, "### %s", n.relPath)
	if n.score > 0 {
		fmt.Fprintf(b, " (score %d)", n.score)
	}
	b.WriteString("\n")
	if len(n.tags) > 0 {
		fmt.Fprintf(b, "tags: %s\n", strings.Join(n.tags, ", "))
	}
	snippet := strings.TrimSpace(n.body)
	if len(snippet) > contextSnippetMaxBytes {
		snippet = snippet[:contextSnippetMaxBytes] + "…"
	}
	if snippet != "" {
		b.WriteString(snippet)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// capOutput truncates the rendered context if it would blow the
// per-source budget. We cut at the next newline so the prompt
// doesn't end mid-sentence.
func capOutput(s string) string {
	if len(s) <= contextOutputMaxBytes {
		return s
	}
	cut := contextOutputMaxBytes
	if nl := strings.LastIndex(s[:cut], "\n"); nl > 0 {
		cut = nl
	}
	return s[:cut] + "\n…(truncated)\n"
}

// tokenizeGoal lowercases the goal, splits on whitespace and
// punctuation that shouldn't anchor a search, drops tokens shorter
// than contextMinTokenLen, and dedupes. The list determines what we
// search for in note bodies/tags/filenames.
func tokenizeGoal(goal string) []string {
	if goal == "" {
		return nil
	}
	lower := strings.ToLower(goal)
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r',
			',', '.', ';', ':', '!', '?',
			'(', ')', '[', ']', '{', '}', '"', '\'',
			'/', '\\', '|', '&', '#', '@', '*':
			return true
		}
		return false
	})
	seen := map[string]struct{}{}
	var out []string
	for _, t := range tokens {
		if len(t) < contextMinTokenLen {
			continue
		}
		if _, ok := stopwords[t]; ok {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// stopwords is a tiny exclusion list — words so common they would
// match every note and dilute the score signal. Keep it minimal; the
// score is already tolerant of noise via per-token weighting.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {},
	"this": {}, "that": {}, "from": {}, "into": {},
	"have": {}, "has": {}, "are": {}, "was": {},
	"will": {}, "would": {}, "should": {},
	"about": {}, "what": {}, "when": {}, "where": {},
	"how": {}, "why": {}, "your": {}, "you": {},
	"can": {},
}
