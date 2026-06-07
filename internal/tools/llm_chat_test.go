package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/secrets"
)

type profilesStub struct {
	profiles map[string]*domain.ProviderProfile
}

func (p *profilesStub) GetByID(id string) (*domain.ProviderProfile, error) {
	return p.profiles[id], nil
}

type settingsStub struct{ pid, mid string }

func (s *settingsStub) GetLLMDefault() (string, string) { return s.pid, s.mid }

type secretStub struct {
	mu   sync.Mutex
	data map[string]string
}

func (s *secretStub) Put(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.data[k] = v
	return nil
}
func (s *secretStub) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *secretStub) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, k)
	return nil
}

func TestLLMChatToolExecute(t *testing.T) {
	var seenSystem, seenUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []llm.ChatMessage `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				seenSystem = m.Content
			}
			if m.Role == "user" {
				seenUser = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":4,"completion_tokens":1},"model":"m1"}`)
	}))
	defer srv.Close()

	profiles := &profilesStub{profiles: map[string]*domain.ProviderProfile{
		"p1": {ID: "p1", Enabled: true, Endpoint: srv.URL, ModelIDs: []string{"m1"}},
	}}
	resolver := llm.NewResolver(profiles, &settingsStub{pid: "p1", mid: "m1"}, &secretStub{})

	tool := NewLLMChatTool(resolver)
	if tool.Name() != "llm.chat" || tool.Capability() != "llm.chat" {
		t.Fatalf("tool identity: %q/%q", tool.Name(), tool.Capability())
	}

	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"prompt":        "ping",
		"system_prompt": "you are terse",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out["output"] != "pong" {
		t.Fatalf("output: %v", out["output"])
	}
	if seenUser != "ping" {
		t.Fatalf("user: %q", seenUser)
	}
	if seenSystem != "you are terse" {
		t.Fatalf("system: %q", seenSystem)
	}
}

func TestLLMChatToolFallsBackToCommandKey(t *testing.T) {
	// Confirm the back-compat path: when the caller passes "command" (from
	// the legacy planSteps), it's treated as the prompt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"model":"m"}`)
	}))
	defer srv.Close()

	profiles := &profilesStub{profiles: map[string]*domain.ProviderProfile{
		"p1": {ID: "p1", Enabled: true, Endpoint: srv.URL, ModelIDs: []string{"m"}},
	}}
	resolver := llm.NewResolver(profiles, &settingsStub{pid: "p1", mid: "m"}, &secretStub{})
	tool := NewLLMChatTool(resolver)

	if _, err := tool.Execute(context.Background(), map[string]interface{}{
		"command": "some goal text",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestLLMChatToolRejectsEmptyPrompt(t *testing.T) {
	resolver := llm.NewResolver(&profilesStub{}, &settingsStub{}, &secretStub{})
	tool := NewLLMChatTool(resolver)
	if _, err := tool.Execute(context.Background(), map[string]interface{}{}); err == nil {
		t.Fatal("expected empty-prompt error")
	}
}

func TestLLMChatToolWithoutResolverFails(t *testing.T) {
	tool := NewLLMChatTool(nil)
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"prompt": "x"}); err == nil {
		t.Fatal("expected error when no resolver is configured")
	}
}

func TestLLMChatToolNoDefaultProvider(t *testing.T) {
	// Resolver constructed but no default configured — tool should return
	// a helpful error rather than panic.
	resolver := llm.NewResolver(&profilesStub{}, &settingsStub{}, &secretStub{})
	tool := NewLLMChatTool(resolver)
	_, err := tool.Execute(context.Background(), map[string]interface{}{"prompt": "hi"})
	if err == nil {
		t.Fatal("expected 'no default provider' error")
	}
}
