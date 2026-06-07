package skills

import (
	"context"
	"math"
	"sort"

	"go.klarlabs.de/nomi/internal/llm"
)

// embeddingThreshold is the cosine-similarity cutoff above which two
// goal embeddings count as "the same skill." Tuned conservatively;
// 0.78 was the empirical sweet spot in pre-release fixtures with
// text-embedding-3-small. Lower thresholds collapse distinct skills,
// higher thresholds fragment the same skill across micro-clusters.
const embeddingThreshold = 0.78

// inducerWithEmbeddings is the embedding-backed clustering path. The
// caller passes a non-nil EmbeddingClient via Config; we batch-embed
// every goal once, normalize, then run the same greedy clustering
// algorithm as the Jaccard path with cosine similarity as the metric.
//
// Falls back to the heuristic path on any embedding error — the
// clustering is a "make suggestions better" optimisation, not a load-
// bearing requirement.
func inducerWithEmbeddings(
	ctx context.Context,
	client llm.EmbeddingClient,
	goals []string,
	threshold float64,
) ([][]int, bool) {
	if client == nil || len(goals) == 0 {
		return nil, false
	}
	vectors, err := client.Embed(ctx, goals)
	if err != nil || len(vectors) != len(goals) {
		return nil, false
	}
	normalized := make([][]float32, len(vectors))
	for i, v := range vectors {
		normalized[i] = normalize(v)
	}
	if threshold <= 0 {
		threshold = embeddingThreshold
	}
	return greedyEmbeddingCluster(normalized, float32(threshold)), true
}

// greedyEmbeddingCluster mirrors greedyCluster but uses cosine
// similarity on normalised vectors. Same O(n^2) shape, same
// "first unvisited element seeds the cluster" pass.
func greedyEmbeddingCluster(vectors [][]float32, threshold float32) [][]int {
	visited := make([]bool, len(vectors))
	clusters := [][]int{}
	for i := range vectors {
		if visited[i] {
			continue
		}
		cluster := []int{i}
		visited[i] = true
		for j := i + 1; j < len(vectors); j++ {
			if visited[j] {
				continue
			}
			if cosine(vectors[i], vectors[j]) >= threshold {
				cluster = append(cluster, j)
				visited[j] = true
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// cosine is the dot product of two pre-normalised vectors. The caller
// must have normalised; the function does NOT re-check magnitude.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// normalize returns a unit-length copy of v. Returns the original
// slice unchanged when the norm rounds to zero — that means the
// vector encoded nothing meaningful anyway and the downstream cosine
// will collapse to 0.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	inv := float32(1.0 / norm)
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// pickEmbeddingCentroid is the embedding-space analogue of
// pickCentroid: the member whose summed cosine to every other member
// is highest. Cheap because clusters are small.
func pickEmbeddingCentroid(members []int, vectors [][]float32) int {
	if len(members) == 1 {
		return members[0]
	}
	best := members[0]
	bestSum := float32(-1.0)
	for _, m := range members {
		var sum float32
		for _, n := range members {
			if m == n {
				continue
			}
			sum += cosine(vectors[m], vectors[n])
		}
		if sum > bestSum {
			bestSum = sum
			best = m
		}
	}
	return best
}

// intersectTokensFromGoals re-runs the Jaccard tokeniser over the
// cluster's goal strings and returns the cross-intersection. Used by
// the embedding path to produce "common tokens" for the synthesised
// recipe even when the clustering itself didn't use tokens.
func intersectTokensFromGoals(members []int, goals []string) []string {
	if len(members) == 0 {
		return nil
	}
	sets := make([]map[string]struct{}, len(goals))
	for i, g := range goals {
		sets[i] = tokenize(g)
	}
	return intersectTokens(members, sets)
}

// sortClustersByMostInformative orders clusters so the highest-cohesion
// ones surface first. Cohesion = average pairwise cosine; ties broken
// by cluster size. The result is what the UI ranks before truncating
// to MaxSuggestions.
func sortClustersByMostInformative(clusters [][]int, vectors [][]float32) {
	sort.SliceStable(clusters, func(i, j int) bool {
		ci := avgPairCosine(clusters[i], vectors)
		cj := avgPairCosine(clusters[j], vectors)
		if ci == cj {
			return len(clusters[i]) > len(clusters[j])
		}
		return ci > cj
	})
}

func avgPairCosine(members []int, vectors [][]float32) float32 {
	if len(members) < 2 {
		return 1.0
	}
	var sum float32
	var pairs float32
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			sum += cosine(vectors[members[i]], vectors[members[j]])
			pairs++
		}
	}
	if pairs == 0 {
		return 0
	}
	return sum / pairs
}
