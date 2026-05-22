package mnemos

import (
	"context"
	"fmt"

	mnemosclient "github.com/felixgeelhaar/mnemos/client"

	"github.com/felixgeelhaar/nomi/internal/plugins"
)

// claimsContextSource implements plugins.ContextSource for the
// mnemos.claims surface. One instance per Connection; the planner
// invokes Query(ctx, goal) at run start and concatenates the result
// into the planner prompt.
//
// Backed by client.Context upstream — Mnemos returns a pre-rendered
// Context Block string that drops straight into a system prompt.
type claimsContextSource struct {
	connID  string
	runID   string // populated when the source is materialized per-run
	maxToks int
	client  *mnemosclient.Client
}

func (s *claimsContextSource) ConnectionID() string { return s.connID }
func (s *claimsContextSource) Name() string         { return ContextSourceName }

func (s *claimsContextSource) Query(ctx context.Context, goal string) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("%s: nil client", ContextSourceName)
	}
	// runID is required by the upstream endpoint. The runtime supplies
	// it via the wrapping ContextSource lookup; if we got here without
	// one, the planner gets no extra context (rather than a 400).
	if s.runID == "" {
		return "", nil
	}
	return s.client.Context(ctx, s.runID, mnemosclient.ContextOptions{
		Query:     goal,
		MaxTokens: s.maxToks,
	})
}

// ContextSources returns one ContextSource per configured Connection.
// The planner picks the source by ConnectionID via the existing
// assistant-binding mechanism.
//
// runID-per-source plumbing is a follow-up: the current
// plugins.ContextSource interface doesn't carry runID through Query,
// so this implementation always returns an empty string until that
// wiring lands. Tools (mnemos.search, claims.list) cover the
// retrieval use case in the meantime.
func (p *Plugin) ContextSources() []plugins.ContextSource {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]plugins.ContextSource, 0, len(p.connections))
	for _, c := range p.connections {
		out = append(out, &claimsContextSource{
			connID:  c.id,
			maxToks: 2000, // conservative default — Mnemos truncates server-side
			client:  c.client,
		})
	}
	return out
}
