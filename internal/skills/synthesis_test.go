package skills

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
	return llm.ChatResponse{Content: f.response, Model: "fake"}, nil
}

func (f *fakeLLM) Provider() string { return "fake" }

func TestSynthesizeHappyPath(t *testing.T) {
	llmClient := &fakeLLM{response: `{
		"suggested_name": "Build & Test Loop",
		"suggested_role": "software engineer",
		"system_prompt": "You build and test code changes. Always run the tests after any edit.",
		"capabilities": ["filesystem.read", "filesystem.write", "command.exec"],
		"explanation": "Captures repeated build-and-test workflows."
	}`}
	suggestion := Suggestion{
		ID:                 "sug1",
		RepresentativeGoal: "build the project and run the tests",
		CommonTokens:       []string{"build", "project", "tests"},
		SourceRunIDs:       []string{"r1", "r2", "r3"},
		Size:               3,
	}
	goals := []string{
		"build the project and run the tests",
		"build project and run tests now",
		"run the tests and build the project",
	}
	out, err := Synthesize(context.Background(), llmClient, suggestion, goals)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if out.SuggestedName != "Build & Test Loop" {
		t.Errorf("name: %q", out.SuggestedName)
	}
	if len(out.Capabilities) != 3 {
		t.Errorf("expected 3 capabilities, got %v", out.Capabilities)
	}
	if !llmClient.lastReq.JSONMode {
		t.Error("expected JSONMode=true")
	}
}

func TestSynthesizeFiltersUnknownCapabilities(t *testing.T) {
	llmClient := &fakeLLM{response: `{
		"suggested_name": "x",
		"system_prompt": "do stuff",
		"capabilities": ["filesystem.read", "fake.capability", "delete.universe"],
		"explanation": "x"
	}`}
	out, err := Synthesize(context.Background(), llmClient, Suggestion{Size: 3}, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(out.Capabilities) != 1 || out.Capabilities[0] != "filesystem.read" {
		t.Fatalf("expected filtered to [filesystem.read], got %v", out.Capabilities)
	}
}

func TestSynthesizeFallbackOnZeroCapabilities(t *testing.T) {
	llmClient := &fakeLLM{response: `{
		"suggested_name": "x",
		"system_prompt": "do stuff",
		"capabilities": ["fake.one", "fake.two"],
		"explanation": "x"
	}`}
	out, err := Synthesize(context.Background(), llmClient, Suggestion{Size: 3}, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	// Should fall back to safe defaults rather than emit zero-capability assistant.
	if len(out.Capabilities) == 0 {
		t.Fatal("expected fallback capabilities")
	}
}

func TestSynthesizeRejectsEmptySystemPrompt(t *testing.T) {
	llmClient := &fakeLLM{response: `{"suggested_name":"x","system_prompt":"   ","capabilities":["llm.chat"],"explanation":"x"}`}
	if _, err := Synthesize(context.Background(), llmClient, Suggestion{Size: 3}, []string{"g"}); err == nil {
		t.Fatal("expected error for empty system_prompt")
	}
}

func TestSynthesizeRequiresClientAndGoals(t *testing.T) {
	if _, err := Synthesize(context.Background(), nil, Suggestion{Size: 3}, []string{"g"}); err == nil {
		t.Fatal("expected error on nil client")
	}
	if _, err := Synthesize(context.Background(), &fakeLLM{}, Suggestion{Size: 3}, nil); err == nil {
		t.Fatal("expected error on empty goals")
	}
}

func TestSynthesizeStripCodeFences(t *testing.T) {
	wrapped := "```json\n{\"suggested_name\":\"x\",\"system_prompt\":\"sp\",\"capabilities\":[\"llm.chat\"],\"explanation\":\"x\"}\n```"
	out, err := Synthesize(context.Background(), &fakeLLM{response: wrapped}, Suggestion{Size: 3}, []string{"g"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if out.SuggestedName != "x" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestSynthesizePropagatesLLMError(t *testing.T) {
	llmClient := &fakeLLM{err: errors.New("boom")}
	if _, err := Synthesize(context.Background(), llmClient, Suggestion{Size: 3}, []string{"g"}); err == nil {
		t.Fatal("expected error to propagate")
	}
}
