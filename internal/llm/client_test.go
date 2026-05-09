package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIClientChatHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Fatalf("bad auth: %q", auth)
		}

		body, _ := io.ReadAll(r.Body)
		var req openaiChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad req body: %v", err)
		}
		if req.Model != "gpt-test" {
			t.Fatalf("model mismatch: %s", req.Model)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Fatalf("message shape: %+v", req.Messages)
		}

		resp := openaiChatResponse{Model: "gpt-test"}
		resp.Choices = []struct {
			Message ChatMessage `json:"message"`
		}{
			{Message: ChatMessage{Role: "assistant", Content: "hello back"}},
		}
		resp.Usage.PromptTokens = 7
		resp.Usage.CompletionTokens = 3
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewClient(Config{Type: EndpointOpenAI, BaseURL: srv.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model: "gpt-test",
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "hello back" {
		t.Fatalf("content: %q", resp.Content)
	}
	if resp.PromptTokens != 7 || resp.OutputTokens != 3 {
		t.Fatalf("usage: %+v", resp)
	}
}

func TestOpenAIClientSurfacesStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"auth"}}`)
	}))
	defer srv.Close()

	client, _ := NewClient(Config{Type: EndpointOpenAI, BaseURL: srv.URL, APIKey: "sk-test"})
	_, err := client.Chat(context.Background(), ChatRequest{
		Model:    "x",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("expected structured error surfaced; got %v", err)
	}
}

func TestAnthropicClientSplitsSystemPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "ant-key" {
			t.Fatalf("bad auth header: %q", r.Header.Get("x-api-key"))
		}
		body, _ := io.ReadAll(r.Body)
		var req anthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.System != "be helpful" {
			t.Fatalf("system not split out: %q", req.System)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Fatalf("user messages: %+v", req.Messages)
		}

		resp := anthropicResponse{Model: "claude-test"}
		resp.Content = []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{
			{Type: "text", Text: "claude response"},
		}
		resp.Usage.InputTokens = 5
		resp.Usage.OutputTokens = 2
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewClient(Config{Type: EndpointAnthropic, BaseURL: srv.URL, APIKey: "ant-key"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model: "claude-test",
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "claude response" {
		t.Fatalf("content: %q", resp.Content)
	}
}

func TestAnthropicClientChatStreamEmitsTextDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// Sanity-check that the streaming flag was actually set in the body.
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Fatalf("expected stream:true in body, got %s", body)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// Anthropic SSE shape: each event has its own type + a JSON data
		// line. Token text only ships in content_block_delta frames whose
		// delta.type is text_delta. Other event types (message_start,
		// content_block_start, ping, message_delta, message_stop) are
		// noise as far as user-visible tokens go.
		frames := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"m1","model":"claude-test","usage":{"input_tokens":4,"output_tokens":0}}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

`,
			`event: message_stop
data: {"type":"message_stop"}

`,
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client, err := NewClient(Config{Type: EndpointAnthropic, BaseURL: srv.URL, APIKey: "ant-key"})
	if err != nil {
		t.Fatal(err)
	}
	streaming, ok := client.(StreamingClient)
	if !ok {
		t.Fatal("anthropic client should implement StreamingClient")
	}

	var deltas []string
	resp, err := streaming.ChatStream(context.Background(), ChatRequest{
		Model:    "claude-test",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := strings.Join(deltas, "|"); got != "hello |world" {
		t.Fatalf("deltas=%q want 'hello |world'", got)
	}
	if resp.Content != "hello world" {
		t.Fatalf("content=%q", resp.Content)
	}
	if resp.Model != "claude-test" {
		t.Fatalf("model=%q", resp.Model)
	}
	if resp.PromptTokens != 4 || resp.OutputTokens != 2 {
		t.Fatalf("usage: prompt=%d output=%d", resp.PromptTokens, resp.OutputTokens)
	}
}

func TestEndpointTypeFor(t *testing.T) {
	cases := []struct {
		endpoint string
		want     EndpointType
	}{
		{"https://api.anthropic.com/v1", EndpointAnthropic},
		{"https://api.openai.com/v1", EndpointOpenAI},
		{"http://localhost:11434/v1", EndpointOpenAI},
	}
	for _, c := range cases {
		if got := endpointTypeFor(c.endpoint); got != c.want {
			t.Errorf("endpointTypeFor(%q) = %s, want %s", c.endpoint, got, c.want)
		}
	}
}

func TestNewClientRequiresBaseURL(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

// TestProviderLabels covers the metrics-attribution heuristic. The
// openai-compat adapter handles several real backends; the label
// must be stable per backend so a Grafana panel can split them.
func TestProviderLabels(t *testing.T) {
	cases := []struct {
		name     string
		endpoint EndpointType
		baseURL  string
		want     string
	}{
		{"anthropic", EndpointAnthropic, "https://api.anthropic.com/v1", "anthropic"},
		{"openai", EndpointOpenAI, "https://api.openai.com/v1", "openai"},
		{"ollama-localhost", EndpointOpenAI, "http://localhost:11434/v1", "ollama"},
		{"ollama-127", EndpointOpenAI, "http://127.0.0.1:11434/v1", "ollama"},
		{"openai-compat-fork", EndpointOpenAI, "https://api.together.xyz/v1", "openai-compat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, err := NewClient(Config{Type: c.endpoint, BaseURL: c.baseURL, APIKey: "x"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if got := client.Provider(); got != c.want {
				t.Errorf("Provider() = %q, want %q", got, c.want)
			}
		})
	}
}
