package learning

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/llm"
)

type fakeLLM struct {
	response string
	err      error
	lastReq  llm.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.lastReq = req
	if f.err != nil {
		return llm.ChatResponse{}, f.err
	}
	return llm.ChatResponse{Content: f.response}, nil
}

func (f *fakeLLM) Provider() string { return "fake" }

func TestExtractHappyPath(t *testing.T) {
	c := &fakeLLM{response: `{"preferences":["Run tests before committing","Prefer yarn over npm"]}`}
	out, err := extract(context.Background(), c, "build and test the project")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 prefs, got %v", out)
	}
	if !c.lastReq.JSONMode {
		t.Error("expected JSONMode=true")
	}
}

func TestExtractEmptyGoal(t *testing.T) {
	out, err := extract(context.Background(), &fakeLLM{}, "   ")
	if err != nil || len(out) != 0 {
		t.Fatalf("expected no prefs for empty goal, got %v err=%v", out, err)
	}
}

func TestExtractPropagatesLLMError(t *testing.T) {
	c := &fakeLLM{err: errors.New("boom")}
	if _, err := extract(context.Background(), c, "goal"); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestExtractStripsCodeFences(t *testing.T) {
	wrapped := "```json\n{\"preferences\":[\"x\"]}\n```"
	out, err := extract(context.Background(), &fakeLLM{response: wrapped}, "g")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(out) != 1 || out[0] != "x" {
		t.Fatalf("unexpected: %v", out)
	}
}

func TestExtractCapsAtMax(t *testing.T) {
	c := &fakeLLM{response: `{"preferences":["a","b","c","d","e"]}`}
	out, _ := extract(context.Background(), c, "g")
	if len(out) != MaxPreferencesPerRun {
		t.Fatalf("expected cap at %d, got %d", MaxPreferencesPerRun, len(out))
	}
}

func TestExtractDedupesCaseInsensitive(t *testing.T) {
	c := &fakeLLM{response: `{"preferences":["Run tests","RUN TESTS","run tests"]}`}
	out, _ := extract(context.Background(), c, "g")
	if len(out) != 1 {
		t.Fatalf("expected dedup to 1, got %v", out)
	}
}

func TestExtractDropsOverLengthEntries(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	c := &fakeLLM{response: `{"preferences":["short","` + long + `"]}`}
	out, _ := extract(context.Background(), c, "g")
	if len(out) != 1 || out[0] != "short" {
		t.Fatalf("expected only 'short', got %v", out)
	}
}
