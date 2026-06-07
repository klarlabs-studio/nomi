package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/llm"
)

// SynthesizedRecipe is what the LLM-driven synthesis step produces from
// a cluster of similar past Runs. The shape mirrors the subset of
// recipes.AssistantSpec the caller will use to populate the promote
// form — system_prompt becomes the reusable template extracted from
// the cluster's goal patterns.
type SynthesizedRecipe struct {
	SuggestedName string   `json:"suggested_name"`
	SuggestedRole string   `json:"suggested_role"`
	SystemPrompt  string   `json:"system_prompt"`
	Capabilities  []string `json:"capabilities"`
	Explanation   string   `json:"explanation"`
}

// synthesisEnvelope models the strict JSON the LLM is asked to emit.
// Parsed with the same code-fence-tolerance the NL cron translator
// uses; validation rejects empty system_prompt + ignores unknown
// capabilities outside the allowlisted set.
type synthesisEnvelope struct {
	SuggestedName string   `json:"suggested_name"`
	SuggestedRole string   `json:"suggested_role"`
	SystemPrompt  string   `json:"system_prompt"`
	Capabilities  []string `json:"capabilities"`
	Explanation   string   `json:"explanation"`
}

// allowedCapabilities is the closed set of capability strings the
// synthesizer may propose. The runtime's permission engine already
// understands every value here; rejecting anything outside the set
// keeps a hallucinating LLM from inventing capabilities Nomi can't
// enforce.
var allowedCapabilities = map[string]struct{}{
	"filesystem.read":  {},
	"filesystem.write": {},
	"command.exec":     {},
	"network.egress":   {},
	"llm.chat":         {},
}

const synthesisSystemPrompt = `You synthesize a reusable Assistant Recipe from a cluster of past successful AI-agent run goals.

Output a single JSON object with these keys:
- suggested_name: short title (3-5 words) for the new assistant
- suggested_role: one-line role description ("software engineer", "research analyst", etc.)
- system_prompt: a reusable system prompt the new assistant will run with. Generalize across the cluster — extract the shared intent, do NOT copy any single goal verbatim.
- capabilities: array of capability strings the assistant needs. Choose from: ["filesystem.read", "filesystem.write", "command.exec", "network.egress", "llm.chat"]. Pick the minimum that lets the assistant do the work.
- explanation: one-sentence summary of what this skill captures.

Be conservative with capabilities — prefer read-only and llm.chat unless the cluster shows the agent needs to write files or run commands.
Output ONLY the JSON object. No prose, no code fences.`

// Synthesize turns a Suggestion's source runs into a recipe shape via
// an LLM call. Caller passes the full Suggestion + the goals of every
// source run (looked up from the run repository before invocation).
//
// Returns a SynthesizedRecipe on success; errors propagate the LLM
// call's error so the caller can surface "translator unavailable" the
// same way the NL cron translator does.
func Synthesize(ctx context.Context, client llm.Client, suggestion Suggestion, sourceGoals []string) (*SynthesizedRecipe, error) {
	if client == nil {
		return nil, errors.New("synthesis: llm client is nil")
	}
	if len(sourceGoals) == 0 {
		return nil, errors.New("synthesis: no source goals supplied")
	}

	userBlock := buildUserBlock(suggestion, sourceGoals)
	resp, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: synthesisSystemPrompt},
			{Role: "user", Content: userBlock},
		},
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("synthesis: llm call: %w", err)
	}

	env, err := parseSynthesisEnvelope(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("synthesis: parse: %w", err)
	}
	if strings.TrimSpace(env.SystemPrompt) == "" {
		return nil, errors.New("synthesis: empty system_prompt")
	}

	caps := filterCapabilities(env.Capabilities)
	if len(caps) == 0 {
		// Default to read-only + llm.chat if the model returned a list
		// that filtered down to zero — better than emitting an
		// uncallable assistant.
		caps = []string{"filesystem.read", "llm.chat"}
	}

	return &SynthesizedRecipe{
		SuggestedName: strings.TrimSpace(env.SuggestedName),
		SuggestedRole: strings.TrimSpace(env.SuggestedRole),
		SystemPrompt:  strings.TrimSpace(env.SystemPrompt),
		Capabilities:  caps,
		Explanation:   strings.TrimSpace(env.Explanation),
	}, nil
}

// buildUserBlock formats the cluster's goals + common tokens into a
// concise prompt the LLM can read. Truncates goals at 200 chars to keep
// the prompt within sensible bounds when a goal happens to be a long
// transcript.
func buildUserBlock(s Suggestion, goals []string) string {
	var b strings.Builder
	b.WriteString("Cluster size: ")
	fmt.Fprintf(&b, "%d runs\n", s.Size)
	if len(s.CommonTokens) > 0 {
		b.WriteString("Common tokens across the cluster: ")
		b.WriteString(strings.Join(s.CommonTokens, ", "))
		b.WriteString("\n")
	}
	b.WriteString("Representative goal: ")
	b.WriteString(s.RepresentativeGoal)
	b.WriteString("\n\nAll cluster goals:\n")
	for i, g := range goals {
		if len(g) > 200 {
			g = g[:200] + "…"
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, g)
	}
	return b.String()
}

// parseSynthesisEnvelope mirrors the NL cron translator's lenient
// JSON-with-code-fence parsing.
func parseSynthesisEnvelope(raw string) (*synthesisEnvelope, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)
	var env synthesisEnvelope
	if err := json.Unmarshal([]byte(cleaned), &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// filterCapabilities drops anything the runtime can't enforce.
func filterCapabilities(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if _, ok := allowedCapabilities[c]; !ok {
			continue
		}
		if _, dupe := seen[c]; dupe {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}
