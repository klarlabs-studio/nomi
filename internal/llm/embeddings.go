package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EmbeddingClient produces fixed-dimensional vector representations of
// short text snippets. Used by skill induction's clustering pass and
// (future) semantic memory recall. Implementations are expected to be
// safe for concurrent use.
type EmbeddingClient interface {
	// Embed returns one vector per input string. The slice length must
	// match the input length. Vectors are normalised by the caller, not
	// the implementation, so cosine similarity stays unambiguous.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Provider returns a stable label identifying the provider family
	// — same shape as Client.Provider(); used for metrics labelling.
	Provider() string
	// Dimensions returns the model's output dimensionality. Used by
	// callers that want to short-circuit when a downstream store
	// expects a fixed shape (e.g. Mnemos embeddings).
	Dimensions() int
}

// openaiEmbeddingClient targets the OpenAI-compat /v1/embeddings shape.
// Ollama, OpenAI, Together, vLLM, LM Studio all speak this protocol;
// the model id is whatever the upstream provider advertises (e.g.
// "text-embedding-3-small" for OpenAI, "nomic-embed-text" for Ollama).
type openaiEmbeddingClient struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	http       *http.Client
	provider   string
}

// EmbeddingConfig configures the embedding client.
type EmbeddingConfig struct {
	BaseURL    string // e.g. https://api.openai.com/v1
	APIKey     string
	Model      string        // e.g. "text-embedding-3-small"
	Dimensions int           // expected output dim; 0 = learn from first response
	Timeout    time.Duration // 0 = DefaultEmbeddingTimeout
	Provider   string        // free-form label for metrics ("openai", "ollama", ...)
}

// DefaultEmbeddingTimeout caps an embedding request. Embeddings are
// shorter-lived than chat completions; 30s is comfortable headroom
// for any single batch.
const DefaultEmbeddingTimeout = 30 * time.Second

// NewEmbeddingClient constructs an OpenAI-compat embedding client.
func NewEmbeddingClient(cfg EmbeddingConfig) (EmbeddingClient, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("llm: embedding BaseURL is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("llm: embedding model is required")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultEmbeddingTimeout
	}
	provider := cfg.Provider
	if provider == "" {
		provider = "openai"
	}
	return &openaiEmbeddingClient{
		baseURL:    cfg.BaseURL,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		dimensions: cfg.Dimensions,
		http:       &http.Client{Timeout: timeout},
		provider:   provider,
	}, nil
}

func (c *openaiEmbeddingClient) Provider() string { return c.provider }
func (c *openaiEmbeddingClient) Dimensions() int  { return c.dimensions }

type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed posts a batch of inputs to the upstream /v1/embeddings endpoint
// and returns one vector per input. Empty input returns an empty slice
// without an HTTP round-trip — saves a no-op call on edge cases.
func (c *openaiEmbeddingClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(embeddingRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embeddings: marshal: %w", err)
	}

	url := c.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("embeddings: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &AuthError{Provider: c.provider, Message: string(body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed embeddingResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings: parse response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: provider returned %d vectors for %d inputs", len(parsed.Data), len(texts))
	}

	// Respect the response order — callers depend on positional
	// alignment with the input slice. Most providers honor request
	// order, but the response also carries an Index field; rely on it
	// defensively so a reordering provider doesn't silently corrupt
	// downstream clustering.
	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: out-of-range index %d", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	if c.dimensions == 0 && len(out) > 0 {
		c.dimensions = len(out[0])
	}
	return out, nil
}
