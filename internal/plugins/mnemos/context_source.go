package mnemos

import (
	"context"
	"fmt"

	mnemosclient "github.com/felixgeelhaar/mnemos/client"

	"github.com/felixgeelhaar/nomi/internal/plugins"
)

// claimsContextSource implements plugins.ContextSource for the
// mnemos.claims surface. One instance per Connection; the planner
// invokes Query at run start with the run's ID + the goal, and
// concatenates the result into the planner prompt.
//
// Backed by client.Context upstream — Mnemos returns a pre-rendered
// Context Block string that drops straight into a system prompt.
type claimsContextSource struct {
	connID  string
	maxToks int
	client  *mnemosclient.Client
}

func (s *claimsContextSource) ConnectionID() string { return s.connID }
func (s *claimsContextSource) Name() string         { return ContextSourceName }

func (s *claimsContextSource) Query(ctx context.Context, request plugins.ContextQueryRequest) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("%s: nil client", ContextSourceName)
	}
	// runID is required by the upstream endpoint. Without one the
	// planner gets no extra context rather than a 400.
	if request.RunID == "" {
		return "", nil
	}
	maxToks := request.MaxTokens
	if maxToks == 0 {
		maxToks = s.maxToks
	}
	return s.client.Context(ctx, request.RunID, mnemosclient.ContextOptions{
		Query:     request.Goal,
		MaxTokens: maxToks,
	})
}

// ContextSources returns one ContextSource per configured Connection.
// The planner picks the source by ConnectionID via the existing
// assistant-binding mechanism.
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
