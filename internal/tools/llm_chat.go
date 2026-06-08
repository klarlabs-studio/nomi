package tools

import (
	"context"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/llm"
)

// LLMChatTool bridges the runtime's tool-execution model to the LLM client.
// By making LLM calls a tool, the rest of the runtime doesn't need special
// cases: permissions, constraints, rate limits, and transactional
// state+event all flow through the same executeStep path.
type LLMChatTool struct {
	resolver *llm.Resolver
}

// NewLLMChatTool constructs the tool. The resolver may be nil only for
// tests that register the tool but never invoke it.
func NewLLMChatTool(resolver *llm.Resolver) *LLMChatTool {
	return &LLMChatTool{resolver: resolver}
}

// Name is the registry key.
func (t *LLMChatTool) Name() string { return "llm.chat" }

// Capability is the permission string the engine checks against.
// "llm.chat" is a first-class capability so connector manifests can opt
// into or out of it (a calendar bot probably doesn't need LLM access; a
// research assistant does).
func (t *LLMChatTool) Capability() string { return "llm.chat" }

// Execute sends a chat request.
//
// Expected input:
//
//	prompt          (string, required) — user message
//	system_prompt   (string, optional) — system instructions
//	model           (string, optional) — overrides the default model
//	provider_id     (string, optional) — overrides the default provider
//	max_tokens      (number, optional) — default 1024
//	temperature     (number, optional) — default 0 (deterministic)
//
// Output:
//
//	output          (string) — assistant message content
//	model           (string) — model that produced it (helpful for audit)
//	prompt_tokens   (number)
//	output_tokens   (number)
func (t *LLMChatTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	if t.resolver == nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeNoLLMProvider,
			Title:   "No AI provider set up",
			Message: "Nomi can't talk to an AI model because no provider is configured. Add an AI provider in Settings → AI Providers.",
			Action:  "Open Settings",
		}
	}

	prompt, _ := input["prompt"].(string)
	if prompt == "" {
		// Fall back to the generic "command" key that lifecycle.planSteps
		// populates with the user's goal. This keeps the tool usable
		// without a schema-aware planner while we ship #33 Multi-Step
		// Planning.
		if cmd, ok := input["command"].(string); ok && cmd != "" {
			prompt = cmd
		}
	}
	if prompt == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePlannerFailed,
			Title:   "Empty request",
			Message: "Nomi didn't receive a message to send to the AI. Try rephrasing your request.",
		}
	}

	var client llm.Client
	var model string
	var providerID string
	var err error

	if pid, ok := input["provider_id"].(string); ok && pid != "" {
		providerID = pid
		client, err = t.resolver.ClientForProfile(providerID)
		if err != nil {
			return nil, err
		}
		model = t.resolver.ModelHint(providerID)
	} else {
		client, model, err = t.resolver.DefaultClient()
		if err != nil {
			return nil, err
		}
		if client == nil {
			return nil, &domain.UserError{
				Code:    domain.ErrCodeNoLLMProvider,
				Title:   "No AI provider set up",
				Message: "Nomi can't talk to an AI model because no provider is configured. Add an AI provider in Settings → AI Providers.",
				Action:  "Open Settings",
			}
		}
	}

	if override, ok := input["model"].(string); ok && override != "" {
		model = override
	}
	if model == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeNoLLMProvider,
			Title:   "No AI model selected",
			Message: "Nomi doesn't know which AI model to use. Check your provider settings and make sure at least one model is selected.",
			Action:  "Open Settings",
		}
	}

	var messages []llm.ChatMessage
	if system, ok := input["system_prompt"].(string); ok && system != "" {
		messages = append(messages, llm.ChatMessage{Role: "system", Content: system})
	}
	messages = append(messages, llm.ChatMessage{Role: "user", Content: prompt})

	maxTokens := 1024
	if raw, ok := input["max_tokens"].(float64); ok {
		maxTokens = int(raw)
	} else if raw, ok := input["max_tokens"].(int); ok {
		maxTokens = raw
	}

	temperature := 0.0
	if raw, ok := input["temperature"].(float64); ok {
		temperature = raw
	}

	chatReq := llm.ChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}

	// Stream when both the adapter supports it AND the runtime supplied a
	// delta callback through the input map (the runtime wires this to a
	// step.streaming event). Without the callback there is no consumer
	// for partial tokens; fall through to the synchronous path so the
	// final-output contract is identical either way.
	onDelta, hasDeltaCb := input["__on_delta"].(func(string))
	if streamer, ok := client.(llm.StreamingClient); ok && hasDeltaCb {
		resp, err := streamer.ChatStream(ctx, chatReq, func(delta string) error {
			onDelta(delta)
			return nil
		})
		if err != nil {
			// Invalidate cache on auth error. For the default client
			// (empty providerID) invalidation is handled by the
			// resolver's DefaultClient path, so there's nothing to do.
			if providerID != "" {
				t.resolver.InvalidateCacheIfAuthError(providerID, err)
			}
			return nil, err
		}
		return map[string]interface{}{
			"output":        resp.Content,
			"model":         resp.Model,
			"prompt_tokens": resp.PromptTokens,
			"output_tokens": resp.OutputTokens,
		}, nil
	}

	resp, err := client.Chat(ctx, chatReq)
	if err != nil {
		// Invalidate cache on auth error
		if providerID != "" {
			t.resolver.InvalidateCacheIfAuthError(providerID, err)
		}
		return nil, err
	}

	return map[string]interface{}{
		"output":        resp.Content,
		"model":         resp.Model,
		"prompt_tokens": resp.PromptTokens,
		"output_tokens": resp.OutputTokens,
	}, nil
}
