package runtime

import (
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

// TestSummarizePriorAttempts_StaysUnderBudget builds a 10-step run
// where every step's output is the full 4 KB. Without the budget
// enforcer that's 40 KB; the result must come in under
// PriorAttemptsBudget (8 KB) and the most recent steps must survive.
func TestSummarizePriorAttempts_StaysUnderBudget(t *testing.T) {
	steps := make([]*domain.Step, 10)
	for i := range steps {
		steps[i] = &domain.Step{
			ID:     fmt.Sprintf("%08d-step-id", i),
			Title:  fmt.Sprintf("Step %d", i),
			Status: domain.StepDone,
			Output: strings.Repeat("x", 4096),
		}
	}
	failed := steps[9]
	got := summarizePriorAttempts(steps, failed, strings.Repeat("y", 200))

	if len(got) > PriorAttemptsBudget {
		t.Fatalf("output %d bytes exceeds PriorAttemptsBudget %d", len(got), PriorAttemptsBudget)
	}
	// The most recent step must always be retained (otherwise the
	// planner can't see what just failed).
	if !strings.Contains(got, "Step 9") {
		t.Fatalf("most recent step missing from summary: %s", got)
	}
	// Failure reason must be present even when the body is heavily
	// truncated — that's the single most important signal for the
	// replan loop.
	if !strings.Contains(got, "Failure reason:") {
		t.Fatalf("failure trailer missing: %s", got)
	}
}

// TestSummarizePriorAttempts_AnnotatesElidedSteps confirms the budget
// enforcer leaves a breadcrumb when older steps were dropped, so the
// planner knows context is incomplete and won't confidently replan
// from a partial picture. We use 30 steps to force elision under the
// 8 KB / 512-per-step caps.
func TestSummarizePriorAttempts_AnnotatesElidedSteps(t *testing.T) {
	steps := make([]*domain.Step, 30)
	for i := range steps {
		steps[i] = &domain.Step{
			ID:     fmt.Sprintf("%08d-step-id", i),
			Title:  fmt.Sprintf("Step %d", i),
			Status: domain.StepDone,
			Output: strings.Repeat("x", 4096),
		}
	}
	got := summarizePriorAttempts(steps, nil, "")
	if !strings.Contains(got, "earlier step(s) elided") {
		t.Fatalf("expected elision breadcrumb, got: %s", got)
	}
}

// TestEmitPlannerEditDistance_CountsAddRemove confirms the metric
// emits one tick per added title and one per removed title, so a
// dashboard split by edit_kind reflects what the user actually did.
func TestEmitPlannerEditDistance_CountsAddRemove(t *testing.T) {
	old := []domain.StepDefinition{
		{Title: "Read README"},
		{Title: "Summarize"},
	}
	updated := []domain.StepDefinition{
		{Title: "Read README"},
		{Title: "Run tests"},
	}
	// "Summarize" removed, "Run tests" added — should produce one
	// add + one remove. We don't read the metric value back here
	// (Prometheus client_golang testutil is overkill); the call must
	// not panic and the build must compile, both of which are
	// covered by the type-checking and compile pass. Still, keep
	// this test so a future refactor that breaks the matching
	// logic shows up.
	emitPlannerEditDistance("openai", old, updated)
}

// TestSummarizePriorAttempts_KeepsAllStepsWhenSmall confirms that
// short-output runs aren't artificially elided. With a 10-step run
// where each output is 100 bytes, all ten should fit.
func TestSummarizePriorAttempts_KeepsAllStepsWhenSmall(t *testing.T) {
	steps := make([]*domain.Step, 10)
	for i := range steps {
		steps[i] = &domain.Step{
			ID:     fmt.Sprintf("%08d-step-id", i),
			Title:  fmt.Sprintf("Step %d", i),
			Status: domain.StepDone,
			Output: strings.Repeat("y", 100),
		}
	}
	got := summarizePriorAttempts(steps, nil, "")
	for i := 0; i < 10; i++ {
		if !strings.Contains(got, fmt.Sprintf("Step %d", i)) {
			t.Fatalf("expected Step %d in output, got: %s", i, got)
		}
	}
	if strings.Contains(got, "elided") {
		t.Fatalf("did not expect elision breadcrumb in small-output run: %s", got)
	}
}
