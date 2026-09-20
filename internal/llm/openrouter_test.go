package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterExtraHeadersOnChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("HTTP-Referer") == "" || r.Header.Get("X-Title") == "" {
			t.Errorf("missing OpenRouter headers: Referer=%q Title=%q",
				r.Header.Get("HTTP-Referer"), r.Header.Get("X-Title"))
		}
		if r.Header.Get("Authorization") != "Bearer sk-or" {
			t.Errorf("bad auth: %q", r.Header.Get("Authorization"))
		}
		var resp openaiChatResponse
		resp.Model = "openrouter/auto"
		resp.Choices = append(resp.Choices, struct {
			Message ChatMessage `json:"message"`
		}{Message: ChatMessage{Role: "assistant", Content: "ok"}})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewClient(Config{
		Type:         EndpointOpenAI,
		BaseURL:      srv.URL,
		APIKey:       "sk-or",
		ExtraHeaders: ExtraHeadersForEndpoint(OpenRouterDefaultBase),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Chat(context.Background(), ChatRequest{
		Model:    "openrouter/auto",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIsOpenRouterEndpoint(t *testing.T) {
	if !IsOpenRouterEndpoint("https://openrouter.ai/api/v1") {
		t.Fatal("expected openrouter match")
	}
	if IsOpenRouterEndpoint("https://api.openai.com/v1") {
		t.Fatal("openai should not match")
	}
}

func TestExtraHeadersForEndpoint(t *testing.T) {
	h := ExtraHeadersForEndpoint(OpenRouterDefaultBase)
	if h["HTTP-Referer"] == "" || h["X-Title"] != "Nomi" {
		t.Fatalf("headers=%v", h)
	}
	if ExtraHeadersForEndpoint("https://api.openai.com/v1") != nil {
		t.Fatal("openai should have no extra headers")
	}
}
