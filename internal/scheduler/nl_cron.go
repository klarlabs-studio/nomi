package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/llm"
)

// TranslateResult is what the NL translator returns to callers. CronExpr
// is empty when the phrase could not be translated; Explanation always
// carries either the LLM's human-readable summary or the validation
// error.
type TranslateResult struct {
	NLPhrase    string `json:"nl_phrase"`
	CronExpr    string `json:"cron_expr"`
	Explanation string `json:"explanation"`
	Valid       bool   `json:"valid"`
}

// llmCronEnvelope is the JSON shape the model is asked to produce. Kept
// strict so a misbehaving model that returns prose around the JSON
// either fails parsing here (caught by the validation step) or is
// caught by the cron parser. Either way, no malformed expression
// reaches the schedule store.
type llmCronEnvelope struct {
	CronExpr    string `json:"cron_expr"`
	Explanation string `json:"explanation"`
}

// systemPrompt asks the LLM to translate phrases into standard 5-field
// cron expressions only. Few-shot examples cover the common cases —
// daily, weekly with weekday filter, monthly, sub-hourly — that users
// type most. Keeping the example count short bounds prompt tokens; the
// validation step below is the actual safety net.
const systemPrompt = `You translate natural-language schedule phrases into standard 5-field cron expressions (minute hour day-of-month month day-of-week).
Output a single JSON object with exactly two keys: "cron_expr" (the cron expression, or empty string if the phrase can't be expressed in cron) and "explanation" (a one-sentence human-readable description in plain English).
Do not include any text outside the JSON object.

Reference examples:
- "every weekday at 8am" → {"cron_expr":"0 8 * * 1-5","explanation":"At 8:00 AM Monday through Friday"}
- "every Monday at 9:30am" → {"cron_expr":"30 9 * * 1","explanation":"At 9:30 AM every Monday"}
- "first day of every month at noon" → {"cron_expr":"0 12 1 * *","explanation":"At 12:00 PM on the 1st of every month"}
- "every 15 minutes" → {"cron_expr":"*/15 * * * *","explanation":"Every 15 minutes"}
- "every hour during business hours on weekdays" → {"cron_expr":"0 9-17 * * 1-5","explanation":"Every hour from 9 AM through 5 PM on weekdays"}
- "every day at 6:45am and 6:45pm" → {"cron_expr":"","explanation":"Multiple distinct times per day cannot be expressed in a single cron expression"}
- "twice a year" → {"cron_expr":"","explanation":"Yearly cadences are not expressible in standard cron"}

Always emit valid JSON. When uncertain, prefer an empty cron_expr with an explanation over a wrong expression.`

// ErrEmptyPhrase is returned when the caller hands the translator a
// blank string. The translator never calls the LLM in this case so the
// failure mode is deterministic.
var ErrEmptyPhrase = errors.New("nl_cron: phrase is empty")

// TranslateNL converts a natural-language phrase into a cron expression
// using the supplied LLM client. The result is validated through the
// scheduler's cron parser before being returned with Valid=true.
//
// Callers should treat Valid=false as "the user needs to retype" — the
// LLM may have legitimately refused (e.g. "twice a year"), or may have
// produced a malformed expression that the cron parser rejected. The
// Explanation field carries the reason in either case.
func (s *Scheduler) TranslateNL(ctx context.Context, client llm.Client, phrase string) (*TranslateResult, error) {
	trimmed := strings.TrimSpace(phrase)
	if trimmed == "" {
		return nil, ErrEmptyPhrase
	}
	if client == nil {
		return nil, errors.New("nl_cron: llm client is nil")
	}

	resp, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: trimmed},
		},
		Temperature: 0,
		JSONMode:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("nl_cron: llm call failed: %w", err)
	}

	envelope, err := parseEnvelope(resp.Content)
	if err != nil {
		return &TranslateResult{
			NLPhrase:    trimmed,
			Explanation: "Could not parse the translator's response: " + err.Error(),
			Valid:       false,
		}, nil
	}

	if envelope.CronExpr == "" {
		return &TranslateResult{
			NLPhrase:    trimmed,
			Explanation: envelope.Explanation,
			Valid:       false,
		}, nil
	}

	if err := s.ValidateCron(envelope.CronExpr); err != nil {
		return &TranslateResult{
			NLPhrase:    trimmed,
			CronExpr:    envelope.CronExpr,
			Explanation: "Translator produced an invalid cron expression: " + err.Error(),
			Valid:       false,
		}, nil
	}

	return &TranslateResult{
		NLPhrase:    trimmed,
		CronExpr:    envelope.CronExpr,
		Explanation: envelope.Explanation,
		Valid:       true,
	}, nil
}

// parseEnvelope is lenient about whitespace + code fences. Models
// occasionally wrap JSON in ```json … ``` even when asked not to;
// strip those before attempting the parse rather than blaming the
// schedule store for refusing to persist a malformed string.
func parseEnvelope(raw string) (*llmCronEnvelope, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var env llmCronEnvelope
	if err := json.Unmarshal([]byte(cleaned), &env); err != nil {
		return nil, err
	}
	return &env, nil
}
