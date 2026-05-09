package runtime

import (
	"fmt"
	"strings"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// Context window budgets. Numbers are bytes; a typical chat model
// counts a token at ≈4 bytes of English prose so 8 KB ≈ 2k tokens —
// plenty for ten step summaries while leaving headroom for the
// system prompt + tool catalog + few-shot examples + the actual goal.
const (
	// PriorAttemptsBudget caps the entire `<previous_attempts>` block
	// in the planner prompt. Keeps a long-running coding session's
	// step history from crowding out the goal.
	PriorAttemptsBudget = 8 * 1024
	// StepOutputBudget caps a single step's output snippet inside
	// the prior-attempts block. Smaller is better; if a step's stderr
	// truly mattered the planner can re-read the run via memory or
	// step row.
	StepOutputBudget = 512
)

// summarizePriorAttempts produces the trusted=false body that
// describes what's already been tried in this run. Two-level budget:
// each step's output is capped at StepOutputBudget; the total block
// is capped at PriorAttemptsBudget with the most-recent steps kept
// when truncation is needed (a planner trying to fix step 9 cares
// more about steps 7-8 than step 1).
func summarizePriorAttempts(steps []*domain.Step, failed *domain.Step, failureMessage string) string {
	// Build per-step entries first so we can drop oldest if needed.
	entries := make([]string, 0, len(steps))
	for _, s := range steps {
		out := s.Output
		if len(out) > StepOutputBudget {
			out = out[:StepOutputBudget] + "…[truncated]"
		}
		shortID := s.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		entries = append(entries, fmt.Sprintf("- [%s] %s — status=%s\n  output: %s\n", shortID, s.Title, s.Status, out))
	}

	// Compute the trailer first (failure section) so its budget is
	// reserved up front. It's the single most useful piece for the
	// planner.
	var trailer strings.Builder
	if failed != nil {
		shortID := failed.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		fmt.Fprintf(&trailer, "\nFailing step: [%s] %s\n", shortID, failed.Title)
	}
	if failureMessage != "" {
		msg := failureMessage
		if len(msg) > StepOutputBudget*2 {
			msg = msg[:StepOutputBudget*2] + "…[truncated]"
		}
		fmt.Fprintf(&trailer, "Failure reason: %s\n", msg)
	}
	trailerBytes := trailer.Len()

	header := "Previously executed steps:\n"
	available := PriorAttemptsBudget - len(header) - trailerBytes
	if available < 0 {
		available = 0
	}

	// Keep the newest steps within budget. Walk from the tail so
	// recency wins; `kept` lists newest-first then we reverse for
	// stable display order.
	kept := make([]string, 0, len(entries))
	used := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if used+len(entries[i]) > available {
			if i > 0 {
				kept = append(kept, fmt.Sprintf("- […%d earlier step(s) elided to fit context budget…]\n", i))
			}
			break
		}
		kept = append(kept, entries[i])
		used += len(entries[i])
	}
	// Reverse to chronological order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	var b strings.Builder
	b.WriteString(header)
	for _, e := range kept {
		b.WriteString(e)
	}
	b.WriteString(trailer.String())
	return b.String()
}
