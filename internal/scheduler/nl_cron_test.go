package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/llm"
)

type fakeLLM struct {
	response string
	err      error
	calls    int
	lastReq  llm.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return llm.ChatResponse{}, f.err
	}
	return llm.ChatResponse{Content: f.response, Model: "fake"}, nil
}

func (f *fakeLLM) Provider() string { return "fake" }

func TestTranslateNLEmptyPhrase(t *testing.T) {
	s := New(nil, nil)
	if _, err := s.TranslateNL(context.Background(), &fakeLLM{}, ""); !errors.Is(err, ErrEmptyPhrase) {
		t.Fatalf("expected ErrEmptyPhrase, got %v", err)
	}
}

func TestTranslateNLRequiresClient(t *testing.T) {
	s := New(nil, nil)
	if _, err := s.TranslateNL(context.Background(), nil, "every weekday"); err == nil {
		t.Fatal("expected error when client is nil")
	}
}

func TestTranslateNLHappyPath(t *testing.T) {
	s := New(nil, nil)
	llmClient := &fakeLLM{response: `{"cron_expr":"0 8 * * 1-5","explanation":"At 8 AM Mon-Fri"}`}
	res, err := s.TranslateNL(context.Background(), llmClient, "every weekday at 8am")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Valid {
		t.Fatalf("expected Valid=true, got %+v", res)
	}
	if res.CronExpr != "0 8 * * 1-5" {
		t.Fatalf("unexpected cron: %q", res.CronExpr)
	}
	if res.NLPhrase != "every weekday at 8am" {
		t.Fatalf("nl_phrase mismatch: %q", res.NLPhrase)
	}
	if !llmClient.lastReq.JSONMode {
		t.Error("expected JSONMode=true on the LLM request")
	}
}

func TestTranslateNLRejectsInvalidCron(t *testing.T) {
	s := New(nil, nil)
	llmClient := &fakeLLM{response: `{"cron_expr":"definitely not cron","explanation":"oops"}`}
	res, err := s.TranslateNL(context.Background(), llmClient, "phrase")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid {
		t.Fatal("expected Valid=false on invalid cron")
	}
	if !strings.Contains(res.Explanation, "invalid cron expression") {
		t.Errorf("expected validation-error explanation, got %q", res.Explanation)
	}
}

func TestTranslateNLEmptyCronMeansUnexpressible(t *testing.T) {
	s := New(nil, nil)
	llmClient := &fakeLLM{response: `{"cron_expr":"","explanation":"twice a year not expressible"}`}
	res, err := s.TranslateNL(context.Background(), llmClient, "twice a year")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid {
		t.Fatal("empty cron should be invalid")
	}
	if !strings.Contains(res.Explanation, "twice a year") {
		t.Errorf("explanation lost: %q", res.Explanation)
	}
}

func TestTranslateNLStripCodeFences(t *testing.T) {
	s := New(nil, nil)
	wrapped := "```json\n{\"cron_expr\":\"*/15 * * * *\",\"explanation\":\"every 15m\"}\n```"
	llmClient := &fakeLLM{response: wrapped}
	res, err := s.TranslateNL(context.Background(), llmClient, "every 15 minutes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Valid {
		t.Fatalf("expected fence-wrapped JSON to parse, got %+v", res)
	}
	if res.CronExpr != "*/15 * * * *" {
		t.Fatalf("cron extracted wrong: %q", res.CronExpr)
	}
}
