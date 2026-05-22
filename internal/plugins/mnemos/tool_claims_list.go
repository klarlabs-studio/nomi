package mnemos

import (
	"context"
	"fmt"
)

// claimsListTool implements mnemos.claims.list. Capability: mnemos.read.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "type": "decision",   // optional
//	  "status": "active",   // optional
//	  "limit": 25,          // optional, server default applies
//	  "offset": 0           // optional
//	}
type claimsListTool struct{ p *Plugin }

func (t *claimsListTool) Name() string       { return ToolClaimsList }
func (t *claimsListTool) Capability() string { return CapRead }

func (t *claimsListTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	b := conn.client.Claims()
	if v, ok := input["type"].(string); ok && v != "" {
		b = b.Type(v)
	}
	if v, ok := input["status"].(string); ok && v != "" {
		b = b.Status(v)
	}
	if v, ok := intFromInput(input, "limit"); ok {
		b = b.Limit(v)
	}
	if v, ok := intFromInput(input, "offset"); ok {
		b = b.Offset(v)
	}

	resp, err := b.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolClaimsList, err)
	}

	return map[string]interface{}{
		"claims":   resp.Claims,
		"evidence": resp.Evidence,
		"total":    resp.Total,
		"limit":    resp.Limit,
		"offset":   resp.Offset,
	}, nil
}

// intFromInput accepts both int and float64 (json.Unmarshal default
// for JSON numbers) and returns the integer value. Returns false if
// the key is absent or the value isn't numeric.
func intFromInput(m map[string]interface{}, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
