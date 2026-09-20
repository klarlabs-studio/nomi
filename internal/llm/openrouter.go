package llm

import "strings"

// OpenRouterDefaultBase is the OpenAI-compatible API root for OpenRouter.
const OpenRouterDefaultBase = "https://openrouter.ai/api/v1"

// openRouterAttributionHeaders are the optional headers OpenRouter asks
// clients to send for rankings / app attribution. Safe defaults —
// operators can still point a generic openai-compat profile at any URL.
func openRouterAttributionHeaders() map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/klarlabs-studio/nomi",
		"X-Title":      "Nomi",
	}
}

// IsOpenRouterEndpoint reports whether the profile endpoint should get
// OpenRouter attribution headers and the "openrouter" metrics label.
func IsOpenRouterEndpoint(endpoint string) bool {
	return strings.Contains(strings.ToLower(endpoint), "openrouter.ai")
}

// ExtraHeadersForEndpoint returns provider-specific headers for the
// given OpenAI-compat endpoint, or nil when none apply.
func ExtraHeadersForEndpoint(endpoint string) map[string]string {
	if IsOpenRouterEndpoint(endpoint) {
		return openRouterAttributionHeaders()
	}
	return nil
}
