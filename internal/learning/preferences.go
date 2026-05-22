// Package learning hosts the auto-learning loop that turns successful
// Runs into reusable preference statements. Subscribed to the event
// bus; on EventRunCompleted it asks the configured LLM to surface
// short, generalizable preferences ("user prefers running tests
// before commits", "always use yarn over npm") and writes them to
// memstore under LocalPreferences scope. The planner already consumes
// LocalPreferences at plan time, so the loop is self-contained.
package learning

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/llm"
	"github.com/felixgeelhaar/nomi/internal/memstore"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// MaxPreferencesPerRun caps how many preference statements one
// completed Run can mint. Bounds prompt-injection blast radius: a
// rogue goal that says "remember everything I say" can't push the
// limit past the cap.
const MaxPreferencesPerRun = 3

// Extractor watches the event bus and writes inferred preferences to
// the configured memory store. Owns its own goroutine for the
// lifetime of the context handed to Start.
type Extractor struct {
	bus       *events.EventBus
	llm       *llm.Resolver
	mem       memstore.Client
	runs      *db.RunRepository
	assistant *db.AssistantRepository

	// MinRunDuration filters out throwaway runs that completed in
	// under N. Mining preferences from a 50ms toy run is noisy and
	// rarely useful; the cap stays generous so legitimate fast
	// successes still feed the loop.
	MinRunDuration time.Duration
}

// New constructs an Extractor. All collaborators are required except
// llm.Resolver, which is required at runtime but tolerated as nil at
// construction so tests can wire up a stub.
func New(bus *events.EventBus, llmResolver *llm.Resolver, mem memstore.Client, runs *db.RunRepository, assistants *db.AssistantRepository) *Extractor {
	return &Extractor{
		bus:            bus,
		llm:            llmResolver,
		mem:            mem,
		runs:           runs,
		assistant:      assistants,
		MinRunDuration: 2 * time.Second,
	}
}

// Start subscribes to RunCompleted events and processes them until
// ctx is cancelled. Errors from individual extractions are logged but
// never propagated; one bad run shouldn't kill the learning loop.
func (e *Extractor) Start(ctx context.Context) {
	if e == nil || e.bus == nil {
		return
	}
	sub := e.bus.Subscribe(events.EventFilter{
		EventTypes: []domain.EventType{domain.EventRunCompleted},
	})
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sub.Events():
				if !ok {
					return
				}
				if err := e.handle(ctx, evt); err != nil {
					slog.Debug("learning: extract failed", "run_id", evt.RunID, "error", err)
				}
			}
		}
	}()
}

func (e *Extractor) handle(ctx context.Context, evt *domain.Event) error {
	if e.llm == nil || e.mem == nil || e.runs == nil {
		return errors.New("learning: collaborators missing")
	}
	run, err := e.runs.GetByID(evt.RunID)
	if err != nil || run == nil {
		return err
	}
	if run.Status != domain.RunCompleted {
		// Defensive — only RunCompleted events are subscribed but the
		// payload may carry stale state on edge cases.
		return nil
	}
	if !run.CreatedAt.IsZero() && time.Since(run.CreatedAt) < e.MinRunDuration {
		// Too-fast runs skipped; usually toy / fixture calls.
		return nil
	}
	client, _, err := e.llm.DefaultClient()
	if err != nil || client == nil {
		// No LLM configured — silent skip is the right behavior.
		return nil
	}

	prefs, err := extract(ctx, client, run.Goal)
	if err != nil {
		return err
	}
	if len(prefs) == 0 {
		return nil
	}
	for _, p := range prefs {
		assistantID := run.AssistantID
		entry := &memstore.Entry{
			Content:     "Inferred: " + p,
			AssistantID: &assistantID,
			RunID:       &run.ID,
		}
		if storeErr := e.mem.Store(ctx, memstore.LocalPreferences(), entry); storeErr != nil {
			slog.Debug("learning: store preference failed", "run_id", run.ID, "error", storeErr)
		}
	}
	return nil
}

// extractEnvelope is the JSON shape the LLM is asked to emit.
type extractEnvelope struct {
	Preferences []string `json:"preferences"`
}

const extractSystemPrompt = `You analyze a completed AI-agent run and surface short, reusable preference statements that should shape the user's future runs.

Output a JSON object with one key, "preferences": an array of 0 to 3 short statements (≤120 characters each) in present tense, written as if the user said them. Each statement should be:
- Specific enough to act on (good: "Run tests before committing"; bad: "Be careful")
- Generalizable across runs (no run-specific names, paths, or dates)
- Independent (don't repeat the same idea in different words)

If the run doesn't reveal any reusable preference, return {"preferences": []}.
Output ONLY the JSON object. No prose, no code fences.`

func extract(ctx context.Context, client llm.Client, goal string) ([]string, error) {
	if strings.TrimSpace(goal) == "" {
		return nil, nil
	}
	resp, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractSystemPrompt},
			{Role: "user", Content: "Goal:\n" + goal},
		},
		Temperature: 0,
		JSONMode:    true,
	})
	if err != nil {
		return nil, err
	}
	env, err := parseEnvelope(resp.Content)
	if err != nil {
		return nil, err
	}
	return clean(env.Preferences), nil
}

func parseEnvelope(raw string) (*extractEnvelope, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)
	var env extractEnvelope
	if err := json.Unmarshal([]byte(cleaned), &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// clean trims, dedupes, caps length + count.
func clean(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 120 {
			continue
		}
		key := strings.ToLower(p)
		if _, dupe := seen[key]; dupe {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
		if len(out) >= MaxPreferencesPerRun {
			break
		}
	}
	return out
}
