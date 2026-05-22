// Package skills implements skill induction from run history (roady #126).
// A "skill" is a candidate Recipe synthesized from a cluster of similar
// past successful runs. Users can review a suggestion and promote it to
// a real Recipe via the registry.
//
// v1 algorithm: heuristic Jaccard-similarity clustering over run goals.
// No embeddings, no LLM-driven prompt synthesis, no parameterized slots
// — those are richer extensions that this scaffolding can grow into
// without changing the wire shape.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// Defaults for the induction pass. Conservative on purpose: a noisier
// signal (too many suggestions, irrelevant clusters) actively makes the
// "Suggested skills" panel useless.
const (
	DefaultMinClusterSize    = 3
	DefaultMinJaccardSim     = 0.5
	DefaultMaxSuggestions    = 10
	DefaultMaxSourceRuns     = 500
)

// Suggestion is the candidate skill the inducer emits. ID is a stable
// hash over the sorted source run IDs so the UI can dedupe and call
// /skills/promote with a reproducible reference.
type Suggestion struct {
	ID                 string   `json:"id"`
	RepresentativeGoal string   `json:"representative_goal"`
	CommonTokens       []string `json:"common_tokens"`
	SourceRunIDs       []string `json:"source_run_ids"`
	Size               int      `json:"size"`
	SuggestedAssistant string   `json:"suggested_assistant_id,omitempty"`
}

// Config tunes the induction pass.
type Config struct {
	MinClusterSize int
	MinJaccardSim  float64
	MaxSuggestions int
	MaxSourceRuns  int
}

// DefaultConfig returns the conservative defaults.
func DefaultConfig() Config {
	return Config{
		MinClusterSize: DefaultMinClusterSize,
		MinJaccardSim:  DefaultMinJaccardSim,
		MaxSuggestions: DefaultMaxSuggestions,
		MaxSourceRuns:  DefaultMaxSourceRuns,
	}
}

// RunSource fetches successful Runs from the runtime's state store.
// Modelled as an interface so tests can substitute a fixed corpus
// without standing up SQLite.
type RunSource interface {
	List(status *domain.RunStatus, limit, offset int) ([]*domain.Run, error)
}

// Induce reads successful Runs from the source and emits zero or more
// Suggestion candidates, sorted by cluster size (largest first).
func Induce(src RunSource, cfg Config) ([]Suggestion, error) {
	completed := domain.RunCompleted
	limit := cfg.MaxSourceRuns
	if limit <= 0 {
		limit = DefaultMaxSourceRuns
	}
	runs, err := src.List(&completed, limit, 0)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}

	minSize := cfg.MinClusterSize
	if minSize <= 0 {
		minSize = DefaultMinClusterSize
	}
	minSim := cfg.MinJaccardSim
	if minSim <= 0 {
		minSim = DefaultMinJaccardSim
	}

	// Precompute token sets per run.
	tokenSets := make([]map[string]struct{}, len(runs))
	for i, r := range runs {
		tokenSets[i] = tokenize(r.Goal)
	}

	clusters := greedyCluster(tokenSets, minSim)

	suggestions := make([]Suggestion, 0)
	for _, members := range clusters {
		if len(members) < minSize {
			continue
		}

		runIDs := make([]string, len(members))
		for j, idx := range members {
			runIDs[j] = runs[idx].ID
		}
		sort.Strings(runIDs)

		// Representative goal: pick the run whose token set has the
		// highest sum of Jaccard-similarities to the other members — a
		// rough centroid. Cheap because the cluster is small.
		repIdx := pickCentroid(members, tokenSets)
		representative := runs[repIdx].Goal
		common := intersectTokens(members, tokenSets)
		assistantID := dominantAssistant(members, runs)

		sum := sha256.Sum256([]byte(strings.Join(runIDs, "|")))
		suggestions = append(suggestions, Suggestion{
			ID:                 hex.EncodeToString(sum[:])[:16],
			RepresentativeGoal: representative,
			CommonTokens:       common,
			SourceRunIDs:       runIDs,
			Size:               len(members),
			SuggestedAssistant: assistantID,
		})
	}

	sort.SliceStable(suggestions, func(i, j int) bool {
		return suggestions[i].Size > suggestions[j].Size
	})

	cap := cfg.MaxSuggestions
	if cap <= 0 {
		cap = DefaultMaxSuggestions
	}
	if len(suggestions) > cap {
		suggestions = suggestions[:cap]
	}
	return suggestions, nil
}

// greedyCluster groups token sets into clusters by Jaccard similarity.
// O(n^2) is fine — n is bounded by MaxSourceRuns (default 500) and the
// pass runs on demand, not in a hot loop.
func greedyCluster(sets []map[string]struct{}, threshold float64) [][]int {
	visited := make([]bool, len(sets))
	clusters := [][]int{}
	for i := range sets {
		if visited[i] {
			continue
		}
		cluster := []int{i}
		visited[i] = true
		for j := i + 1; j < len(sets); j++ {
			if visited[j] {
				continue
			}
			if jaccard(sets[i], sets[j]) >= threshold {
				cluster = append(cluster, j)
				visited[j] = true
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// jaccard returns |A ∩ B| / |A ∪ B|. Returns 0 for two empty sets so a
// pile of goal-less runs doesn't all coalesce into one false cluster.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// pickCentroid returns the index of the member with the highest total
// similarity to every other member in the cluster.
func pickCentroid(members []int, sets []map[string]struct{}) int {
	if len(members) == 1 {
		return members[0]
	}
	best := members[0]
	bestSum := -1.0
	for _, m := range members {
		sum := 0.0
		for _, n := range members {
			if m == n {
				continue
			}
			sum += jaccard(sets[m], sets[n])
		}
		if sum > bestSum {
			bestSum = sum
			best = m
		}
	}
	return best
}

// intersectTokens returns the tokens present in EVERY member's set,
// sorted lexicographically. These are the "common tokens" of the
// cluster — useful for naming the synthesized recipe.
func intersectTokens(members []int, sets []map[string]struct{}) []string {
	if len(members) == 0 {
		return nil
	}
	seed := sets[members[0]]
	candidates := make(map[string]int, len(seed))
	for tok := range seed {
		candidates[tok] = 1
	}
	for _, idx := range members[1:] {
		for tok := range candidates {
			if _, ok := sets[idx][tok]; !ok {
				delete(candidates, tok)
			}
		}
	}
	out := make([]string, 0, len(candidates))
	for tok := range candidates {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// dominantAssistant returns the assistant ID that appears in more than
// half the cluster's runs. Empty when no single assistant dominates —
// callers can fall back to the user's choice at promote time.
func dominantAssistant(members []int, runs []*domain.Run) string {
	counts := map[string]int{}
	for _, idx := range members {
		counts[runs[idx].AssistantID]++
	}
	for id, n := range counts {
		if n*2 > len(members) {
			return id
		}
	}
	return ""
}

// stopwords intentionally short — aggressive trimming would discard
// signal from the (typically domain-specific) goal text users write.
// Tuned by reading actual goal corpora; safe to extend.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {},
	"in": {}, "on": {}, "at": {}, "to": {}, "of": {}, "for": {},
	"with": {}, "from": {}, "by": {}, "as": {}, "is": {}, "are": {},
	"was": {}, "be": {}, "this": {}, "that": {}, "it": {}, "i": {},
	"my": {}, "me": {}, "you": {}, "your": {},
}

// tokenize lowercases, splits on non-alphanumerics, drops stopwords and
// tokens shorter than 3 chars. Returns a set for O(1) membership checks.
func tokenize(text string) map[string]struct{} {
	set := map[string]struct{}{}
	current := strings.Builder{}
	flush := func() {
		if current.Len() == 0 {
			return
		}
		t := current.String()
		current.Reset()
		if len(t) < 3 {
			return
		}
		if _, drop := stopwords[t]; drop {
			return
		}
		set[t] = struct{}{}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			current.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return set
}
