package mnemos

import (
	"context"
	"fmt"
)

// relationshipsListTool implements mnemos.relationships.list. Capability:
// mnemos.read.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "type": "contradicts",  // optional: supports | contradicts
//	  "limit": 25,
//	  "offset": 0
//	}
type relationshipsListTool struct{ p *Plugin }

func (t *relationshipsListTool) Name() string       { return ToolRelationshipsList }
func (t *relationshipsListTool) Capability() string { return CapRead }

func (t *relationshipsListTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	b := conn.client.Relationships()
	if v, ok := input["type"].(string); ok && v != "" {
		b = b.Type(v)
	}
	if v, ok := intFromInput(input, "limit"); ok {
		b = b.Limit(v)
	}
	if v, ok := intFromInput(input, "offset"); ok {
		b = b.Offset(v)
	}

	resp, err := b.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolRelationshipsList, err)
	}

	return map[string]interface{}{
		"relationships": resp.Relationships,
		"total":         resp.Total,
		"limit":         resp.Limit,
		"offset":        resp.Offset,
	}, nil
}
