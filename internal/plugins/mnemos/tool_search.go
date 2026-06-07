package mnemos

import (
	"context"
	"fmt"

	mnemosclient "go.klarlabs.de/mnemos/client"
)

// searchTool implements mnemos.search. Capability: mnemos.read.
//
// Hybrid retrieval (vector + lexical) over the claim store. Maps to
// client.Search upstream.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "query": "rate limiting strategy",
//	  "top_k": 10,                         // optional
//	  "run_id": "...",                     // optional; filters to run
//	  "min_trust": 0.5,                    // optional
//	  "as_of": "2026-05-01T00:00:00Z",     // optional, validity time
//	  "recorded_as_of": "2026-05-22T..."   // optional, ingestion time
//	}
type searchTool struct{ p *Plugin }

func (t *searchTool) Name() string       { return ToolSearch }
func (t *searchTool) Capability() string { return CapRead }

func (t *searchTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	query, _ := input["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("%s: query required", ToolSearch)
	}

	opts := mnemosclient.SearchOptions{}
	if v, ok := input["run_id"].(string); ok {
		opts.RunID = v
	}
	if v, ok := intFromInput(input, "top_k"); ok {
		opts.TopK = v
	}
	if v, ok := input["min_trust"].(float64); ok {
		opts.MinTrust = v
	}
	if v, ok := input["as_of"].(string); ok {
		opts.AsOf = v
	}
	if v, ok := input["recorded_as_of"].(string); ok {
		opts.RecordedAsOf = v
	}

	resp, err := conn.client.Search(ctx, query, opts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolSearch, err)
	}

	return map[string]interface{}{
		"query":           resp.Query,
		"top_k":           resp.TopK,
		"total":           resp.Total,
		"claims":          resp.Claims,
		"contradictions":  resp.Contradictions,
		"applied_filters": resp.AppliedFilters,
	}, nil
}
