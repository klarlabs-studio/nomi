package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ProbeResult captures the outcome of an LLM provider probe. Reachable
// flips false when the HTTP request fails, the response status is non-2xx,
// or the body fails to parse — the caller doesn't need to distinguish
// between those for UX purposes; "couldn't reach" is one error to fix.
type ProbeResult struct {
	Reachable        bool
	ModelsAvailable  []string
	MissingRequested []string
	Error            string
}

// Probe issues a GET <endpoint>/models against the OpenAI-compatible
// provider at the given (already /v1-normalised) endpoint and reports
// which of the requested model IDs the provider lists. Times out at 5s
// so the wizard's save path doesn't block on a flaky remote.
//
// Anthropic does not expose /v1/models publicly, so the probe will
// report Reachable=false for Anthropic endpoints — the UI should treat
// the result as advisory there and surface the message rather than
// blocking save.
func Probe(ctx context.Context, endpoint, apiKey string, requested []string) ProbeResult {
	if endpoint == "" {
		return ProbeResult{Error: "endpoint is empty"}
	}
	url := strings.TrimRight(endpoint, "/") + "/models"
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nomid-probe/0.1")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProbeResult{Error: fmt.Sprintf("http %d", resp.StatusCode)}
	}

	// OpenAI shape: {"data": [{"id":"qwen2.5:14b"}, ...]}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProbeResult{Error: "couldn't parse /models response: " + err.Error()}
	}

	available := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			available = append(available, m.ID)
		}
	}

	missing := make([]string, 0)
	for _, want := range requested {
		found := false
		for _, have := range available {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	return ProbeResult{
		Reachable:        true,
		ModelsAvailable:  available,
		MissingRequested: missing,
	}
}
