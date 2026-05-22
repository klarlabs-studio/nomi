package skills

import (
	"testing"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

type fakeRunSource struct{ runs []*domain.Run }

func (f *fakeRunSource) List(_ *domain.RunStatus, _ int, _ int) ([]*domain.Run, error) {
	return f.runs, nil
}

func TestTokenizeDropsStopwordsAndShortTokens(t *testing.T) {
	got := tokenize("Run the build and the tests in the repo")
	want := map[string]bool{"run": true, "build": true, "tests": true, "repo": true}
	if len(got) != len(want) {
		t.Fatalf("token count mismatch: got %v", got)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing token %q", k)
		}
	}
}

func TestJaccardSymmetric(t *testing.T) {
	a := tokenize("run the build and test the repo")
	b := tokenize("build the test for the repo")
	ab := jaccard(a, b)
	ba := jaccard(b, a)
	if ab != ba {
		t.Fatalf("jaccard not symmetric: %f vs %f", ab, ba)
	}
	if ab <= 0 || ab >= 1 {
		t.Fatalf("expected partial overlap, got %f", ab)
	}
}

func TestInduceProducesClustersAboveThreshold(t *testing.T) {
	src := &fakeRunSource{runs: []*domain.Run{
		{ID: "r1", Goal: "build the project and run the tests", AssistantID: "a1"},
		{ID: "r2", Goal: "build project and run tests now", AssistantID: "a1"},
		{ID: "r3", Goal: "run the tests and build the project", AssistantID: "a1"},
		{ID: "r4", Goal: "draft a blog post about jellyfish", AssistantID: "a2"},
		{ID: "r5", Goal: "summarise the marketing meeting notes", AssistantID: "a2"},
	}}
	suggestions, err := Induce(src, DefaultConfig())
	if err != nil {
		t.Fatalf("induce: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %d: %+v", len(suggestions), suggestions)
	}
	s := suggestions[0]
	if s.Size != 3 {
		t.Errorf("expected cluster size 3, got %d", s.Size)
	}
	if s.SuggestedAssistant != "a1" {
		t.Errorf("expected SuggestedAssistant a1, got %q", s.SuggestedAssistant)
	}
	if len(s.SourceRunIDs) != 3 {
		t.Errorf("expected 3 source runs, got %d", len(s.SourceRunIDs))
	}
	// Common tokens for cluster should include the cluster's stable
	// terms.
	commonHas := func(want string) bool {
		for _, t := range s.CommonTokens {
			if t == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"build", "project", "tests"} {
		if !commonHas(want) {
			t.Errorf("expected common token %q in %v", want, s.CommonTokens)
		}
	}
}

func TestInduceRespectsMinClusterSize(t *testing.T) {
	src := &fakeRunSource{runs: []*domain.Run{
		{ID: "r1", Goal: "alpha beta gamma delta", AssistantID: "a1"},
		{ID: "r2", Goal: "alpha beta gamma delta", AssistantID: "a1"},
		// Only 2 identical runs — below default MinClusterSize=3.
	}}
	suggestions, err := Induce(src, DefaultConfig())
	if err != nil {
		t.Fatalf("induce: %v", err)
	}
	if len(suggestions) != 0 {
		t.Fatalf("expected 0 suggestions (cluster too small), got %d", len(suggestions))
	}
}

func TestInduceStableSuggestionID(t *testing.T) {
	src := &fakeRunSource{runs: []*domain.Run{
		{ID: "r1", Goal: "alpha beta gamma delta epsilon", AssistantID: "a1"},
		{ID: "r2", Goal: "alpha beta gamma delta epsilon", AssistantID: "a1"},
		{ID: "r3", Goal: "alpha beta gamma delta epsilon", AssistantID: "a1"},
	}}
	first, err := Induce(src, DefaultConfig())
	if err != nil {
		t.Fatalf("induce: %v", err)
	}
	second, err := Induce(src, DefaultConfig())
	if err != nil {
		t.Fatalf("induce: %v", err)
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("suggestion ID not stable: %q vs %q", first[0].ID, second[0].ID)
	}
}

func TestInduceEmptyCorpus(t *testing.T) {
	src := &fakeRunSource{runs: nil}
	out, err := Induce(src, DefaultConfig())
	if err != nil {
		t.Fatalf("induce: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 suggestions on empty corpus, got %d", len(out))
	}
}
