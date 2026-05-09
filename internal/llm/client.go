// Package llm is Nomi's abstraction over chat-completion LLM providers.
//
// The package intentionally speaks the OpenAI /chat/completions wire format
// because it's the lingua franca — Ollama, LM Studio, vLLM, Together, and
// OpenAI itself all expose it. Anthropic's native API is supported via the
// `anthropic` endpoint type which rewrites requests to the Messages API
// before sending.
//
// Clients never persist anything and never hold more than the minimum
// configuration in memory. API keys are resolved from the secrets.Store at
// construction time and not re-read, so a client tied to a stale key fails
// with a clear error rather than silently using the wrong credential.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatMessage is a single turn in a chat conversation. Role is one of
// "system", "user", "assistant". The OpenAI schema also permits "tool" and
// "function"; Nomi doesn't emit those today but the field accepts them so
// future tool-use support isn't a breaking change.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is what the runtime asks an LLM to do. Model is required;
// the rest have sensible defaults in the client.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	// Stop is optional and tool-usage-adjacent; kept minimal for V1.
	Stop []string `json:"stop,omitempty"`
	// JSONMode asks adapters that support it (OpenAI / Ollama) to set
	// response_format=json_object so the model emits parseable JSON
	// without prose. Anthropic ignores the flag — its messages API
	// handles JSON via tool-use or prefill, neither of which is wired
	// here yet — so the planner falls back to instruction-following on
	// that path. Set true only when the caller can recover from a
	// silently-text response.
	JSONMode bool `json:"-"`
}

// AuthError is returned when an LLM provider responds with 401 Unauthorized.
// Callers can type-assert to detect auth failures and trigger cache invalidation.
type AuthError struct {
	Provider string
	Message  string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("llm auth error (%s): %s", e.Provider, e.Message)
}

// ChatResponse captures just what callers need. Token counts let callers
// record usage in events; the rest of the OpenAI response schema is
// discarded.
type ChatResponse struct {
	Content      string
	Model        string
	PromptTokens int
	OutputTokens int
}

// Client is the synchronous interface every adapter implements; the
// runtime falls back to it when streaming isn't supported or when the
// caller just wants the final answer.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Provider returns a stable label identifying the provider family
	// ("openai", "anthropic", "ollama", etc.). Used by the planner
	// metrics to attribute success/failure to a specific backend so
	// an Ollama regression doesn't get masked by OpenAI's success
	// rate on the same Grafana panel.
	Provider() string
}

// StreamingClient is implemented by adapters that can emit deltas as the
// model generates them. Callers type-assert to detect support; the
// non-streaming Chat path remains the contract floor so a new adapter
// can ship without streaming. The visit callback runs on the goroutine
// that called ChatStream and may run many times before the function
// returns; if visit returns an error, the stream is cancelled.
type StreamingClient interface {
	ChatStream(ctx context.Context, req ChatRequest, visit func(delta string) error) (ChatResponse, error)
}

// EndpointType identifies which request shape the client emits. Covers the
// OpenAI-compat majority plus Anthropic's native Messages API.
type EndpointType string

const (
	// EndpointOpenAI is the /v1/chat/completions schema. Covers OpenAI,
	// Ollama (with OPENAI_API_BASE), LM Studio, Together, Groq, vLLM, etc.
	EndpointOpenAI EndpointType = "openai"
	// EndpointAnthropic is the /v1/messages Anthropic-native schema.
	EndpointAnthropic EndpointType = "anthropic"
)

// Config is the static data a Client is constructed with. Resolver uses
// this to build concrete Clients from ProviderProfile rows.
type Config struct {
	Type      EndpointType
	BaseURL   string        // e.g. https://api.openai.com/v1
	APIKey    string        // already-resolved plaintext (not a secret:// reference)
	Timeout   time.Duration // 0 → DefaultRequestTimeout
	UserAgent string
}

// DefaultRequestTimeout caps any single LLM call. Long-context requests
// against slow local Ollama models can legitimately take a while; the
// ceiling sits well above the 95th percentile but below "probably hung."
const DefaultRequestTimeout = 120 * time.Second

// NewClient picks the right adapter for the configured EndpointType.
func NewClient(cfg Config) (Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("llm: BaseURL is required")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	httpClient := &http.Client{Timeout: timeout}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "nomid/0.1"
	}
	switch cfg.Type {
	case EndpointAnthropic:
		return &anthropicClient{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, http: httpClient, ua: ua}, nil
	case EndpointOpenAI, "":
		// Treat empty type as OpenAI-compat since that's the default for
		// any OpenAI-compat backend including Ollama.
		return &openaiClient{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, http: httpClient, ua: ua}, nil
	default:
		return nil, fmt.Errorf("llm: unknown endpoint type %q", cfg.Type)
	}
}

// ---- OpenAI-compat adapter ---------------------------------------------

type openaiClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	ua      string
}

// Provider returns a label for the metrics. The openai-compat schema
// covers several real backends; we discriminate by URL because the
// adapter is the same code path. Local 11434 is the Ollama default;
// api.openai.com is OpenAI proper; everything else is collapsed to
// "openai-compat" (Together, Groq, vLLM, LM Studio, etc.) — fine
// because operators usually run one of those at a time and the goal
// is just to separate the bucket, not enumerate every fork.
func (c *openaiClient) Provider() string {
	switch {
	case strings.Contains(c.baseURL, "127.0.0.1:11434") || strings.Contains(c.baseURL, "localhost:11434"):
		return "ollama"
	case strings.Contains(c.baseURL, "api.openai.com"):
		return "openai"
	default:
		return "openai-compat"
	}
}

type openaiChatRequest struct {
	Model          string                  `json:"model"`
	Messages       []ChatMessage           `json:"messages"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`
	Temperature    *float64                `json:"temperature,omitempty"`
	Stop           []string                `json:"stop,omitempty"`
	Stream         bool                    `json:"stream,omitempty"`
	ResponseFormat *openaiResponseFormat   `json:"response_format,omitempty"`
}

type openaiResponseFormat struct {
	Type string `json:"type"`
}

type openaiChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (c *openaiClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := openaiChatRequest{
		Model:     req.Model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
		Stop:      req.Stop,
	}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	if req.JSONMode {
		body.ResponseFormat = &openaiResponseFormat{Type: "json_object"}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.ua)
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: http error: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Surface structured errors when present, fall back to the raw body.
		var structured openaiChatResponse
		if json.Unmarshal(raw, &structured) == nil && structured.Error != nil {
			// Check for 401 Unauthorized - return AuthError for cache invalidation
			if resp.StatusCode == http.StatusUnauthorized {
				return ChatResponse{}, &AuthError{Provider: "openai", Message: structured.Error.Message}
			}
			return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, structured.Error.Message)
		}
		// Check for 401 even without structured error
		if resp.StatusCode == http.StatusUnauthorized {
			return ChatResponse{}, &AuthError{Provider: "openai", Message: string(raw)}
		}
		return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, string(raw))
	}

	var parsed openaiChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("llm: parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("llm: empty choices in response")
	}

	return ChatResponse{
		Content:      parsed.Choices[0].Message.Content,
		Model:        parsed.Model,
		PromptTokens: parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
	}, nil
}

// ChatStream issues a /chat/completions request with stream=true and
// invokes visit() as deltas arrive. Server-Sent-Events frames are
// concatenated into the final ChatResponse content so callers don't need
// to track tokens themselves; the streaming part is purely a UX layer.
//
// On any visit() error or context cancel, the connection is closed and
// the partial accumulation is returned alongside the error.
func (c *openaiClient) ChatStream(
	ctx context.Context,
	req ChatRequest,
	visit func(delta string) error,
) (ChatResponse, error) {
	body := openaiChatRequest{
		Model:     req.Model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
		Stop:      req.Stop,
		Stream:    true,
	}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", c.ua)
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: http error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, string(raw))
	}

	// Each SSE frame on /chat/completions is `data: {json}\n` with deltas
	// like `{"choices":[{"delta":{"content":"hello"}}]}`. The terminator
	// is the literal `data: [DONE]`. We accumulate content and emit each
	// non-empty delta to visit().
	var (
		accumulated string
		modelID     string
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var frame struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			// Skip malformed frames silently; the upstream sometimes
			// emits keep-alive comments that aren't JSON.
			continue
		}
		if frame.Model != "" && modelID == "" {
			modelID = frame.Model
		}
		for _, choice := range frame.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			accumulated += choice.Delta.Content
			if err := visit(choice.Delta.Content); err != nil {
				return ChatResponse{Content: accumulated, Model: modelID}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResponse{Content: accumulated, Model: modelID}, fmt.Errorf("llm: read stream: %w", err)
	}
	return ChatResponse{Content: accumulated, Model: modelID}, nil
}

// ---- Anthropic native adapter ------------------------------------------

type anthropicClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	ua      string
}

// Provider returns the metrics label. Always "anthropic" — there's
// only one wire format wired here.
func (c *anthropicClient) Provider() string { return "anthropic" }

type anthropicRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []ChatMessage `json:"messages"`
	Temp      *float64      `json:"temperature,omitempty"`
	Stop      []string      `json:"stop_sequences,omitempty"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error,omitempty"`
}

func (c *anthropicClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	// Anthropic splits the system prompt out of the messages array.
	var system string
	var userMsgs []ChatMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		userMsgs = append(userMsgs, m)
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  userMsgs,
		Stop:      req.Stop,
	}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temp = &t
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(buf))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: build anthropic request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("User-Agent", c.ua)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: http error: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var structured anthropicResponse
		if json.Unmarshal(raw, &structured) == nil && structured.Error != nil {
			// Check for 401 Unauthorized - return AuthError for cache invalidation
			if resp.StatusCode == http.StatusUnauthorized {
				return ChatResponse{}, &AuthError{Provider: "anthropic", Message: structured.Error.Message}
			}
			return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, structured.Error.Message)
		}
		// Check for 401 even without structured error
		if resp.StatusCode == http.StatusUnauthorized {
			return ChatResponse{}, &AuthError{Provider: "anthropic", Message: string(raw)}
		}
		return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, string(raw))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("llm: parse response: %w", err)
	}

	var out strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return ChatResponse{
		Content:      out.String(),
		Model:        parsed.Model,
		PromptTokens: parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}

// ChatStream issues a /v1/messages request with stream=true and invokes
// visit() as content_block_delta frames arrive. Anthropic's SSE shape is
// distinct from OpenAI's: each frame is `event: <type>\ndata: {...}\n\n`,
// and only events of type content_block_delta with delta.type=text_delta
// carry user-visible tokens. message_stop ends the stream.
//
// On any visit() error or context cancel, the connection is closed and
// the partial accumulation is returned alongside the error. Mirrors the
// openaiClient.ChatStream contract so the runtime can route either
// adapter through the same streaming path.
func (c *anthropicClient) ChatStream(
	ctx context.Context,
	req ChatRequest,
	visit func(delta string) error,
) (ChatResponse, error) {
	var system string
	var userMsgs []ChatMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		userMsgs = append(userMsgs, m)
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	bodyMap := map[string]interface{}{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"messages":   userMsgs,
		"stream":     true,
	}
	if system != "" {
		bodyMap["system"] = system
	}
	if req.Temperature != 0 {
		bodyMap["temperature"] = req.Temperature
	}
	if len(req.Stop) > 0 {
		bodyMap["stop_sequences"] = req.Stop
	}
	buf, err := json.Marshal(bodyMap)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: marshal anthropic stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(buf))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: build anthropic stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("User-Agent", c.ua)
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: http error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return ChatResponse{}, &AuthError{Provider: "anthropic", Message: string(raw)}
		}
		return ChatResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, string(raw))
	}

	var (
		accumulated  strings.Builder
		modelID      string
		inputTokens  int
		outputTokens int
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message,omitempty"`
			Delta struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				StopReason   string `json:"stop_reason,omitempty"`
				OutputTokens int    `json:"output_tokens,omitempty"`
			} `json:"delta,omitempty"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			// Anthropic sends ping events whose data line is empty JSON
			// or non-JSON keep-alives — skip silently.
			continue
		}
		switch frame.Type {
		case "message_start":
			if frame.Message.Model != "" {
				modelID = frame.Message.Model
			}
			inputTokens = frame.Message.Usage.InputTokens
		case "content_block_delta":
			if frame.Delta.Type == "text_delta" && frame.Delta.Text != "" {
				accumulated.WriteString(frame.Delta.Text)
				if err := visit(frame.Delta.Text); err != nil {
					return ChatResponse{
						Content:      accumulated.String(),
						Model:        modelID,
						PromptTokens: inputTokens,
						OutputTokens: outputTokens,
					}, err
				}
			}
		case "message_delta":
			// Final usage arrives here.
			if frame.Usage.OutputTokens > 0 {
				outputTokens = frame.Usage.OutputTokens
			}
		case "message_stop":
			// stream complete; loop will exit on next read.
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResponse{
			Content:      accumulated.String(),
			Model:        modelID,
			PromptTokens: inputTokens,
			OutputTokens: outputTokens,
		}, fmt.Errorf("llm: read stream: %w", err)
	}
	return ChatResponse{
		Content:      accumulated.String(),
		Model:        modelID,
		PromptTokens: inputTokens,
		OutputTokens: outputTokens,
	}, nil
}
