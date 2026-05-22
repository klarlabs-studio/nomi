package mnemos

import (
	"context"
	"fmt"

	mnemosclient "github.com/felixgeelhaar/mnemos/client"
)

// claimsAppendTool implements mnemos.claims.append. Capability:
// mnemos.write.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "claims": [
//	    { "text": "...", "type": "decision", "confidence": 0.9,
//	      "status": "active", "visibility": "team" }
//	  ],
//	  "evidence": [
//	    { "claim_id": "...", "event_id": "..." }
//	  ]
//	}
//
// At least one claim required. Evidence is optional. Visibility falls
// back to the connection's visibility_default when omitted.
type claimsAppendTool struct{ p *Plugin }

func (t *claimsAppendTool) Name() string       { return ToolClaimsAppend }
func (t *claimsAppendTool) Capability() string { return CapWrite }

func (t *claimsAppendTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	rawClaims, ok := input["claims"].([]interface{})
	if !ok || len(rawClaims) == 0 {
		return nil, fmt.Errorf("%s: input.claims must be a non-empty array", ToolClaimsAppend)
	}

	claims := make([]mnemosclient.Claim, 0, len(rawClaims))
	for i, rc := range rawClaims {
		cm, ok := rc.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s: claims[%d] must be an object", ToolClaimsAppend, i)
		}
		claim, err := parseClaim(cm, conn.visibilityDefault)
		if err != nil {
			return nil, fmt.Errorf("%s: claims[%d]: %w", ToolClaimsAppend, i, err)
		}
		claims = append(claims, claim)
	}

	var evidence []mnemosclient.EvidenceLink
	if rawEvidence, ok := input["evidence"].([]interface{}); ok && len(rawEvidence) > 0 {
		evidence = make([]mnemosclient.EvidenceLink, 0, len(rawEvidence))
		for i, re := range rawEvidence {
			em, ok := re.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("%s: evidence[%d] must be an object", ToolClaimsAppend, i)
			}
			cid, _ := em["claim_id"].(string)
			eid, _ := em["event_id"].(string)
			if cid == "" || eid == "" {
				return nil, fmt.Errorf("%s: evidence[%d] requires claim_id and event_id", ToolClaimsAppend, i)
			}
			evidence = append(evidence, mnemosclient.EvidenceLink{ClaimID: cid, EventID: eid})
		}
	}

	resp, err := conn.client.Claims().Append(ctx, claims, evidence)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolClaimsAppend, err)
	}

	return map[string]interface{}{
		"accepted": resp.Accepted,
		"skipped":  resp.Skipped,
	}, nil
}

// parseClaim validates and converts a single input map into a typed
// Claim. visibilityDefault is the connection's fallback when the
// caller doesn't specify visibility explicitly.
func parseClaim(m map[string]interface{}, visibilityDefault string) (mnemosclient.Claim, error) {
	var c mnemosclient.Claim
	text, _ := m["text"].(string)
	if text == "" {
		return c, fmt.Errorf("text required")
	}
	claimType, _ := m["type"].(string)
	if claimType == "" {
		return c, fmt.Errorf("type required (fact | hypothesis | decision | test_result)")
	}
	c.Text = text
	c.Type = claimType
	if id, ok := m["id"].(string); ok {
		c.ID = id
	}
	if conf, ok := m["confidence"].(float64); ok {
		c.Confidence = conf
	}
	if status, ok := m["status"].(string); ok {
		c.Status = status
	}
	if visibility, ok := m["visibility"].(string); ok && visibility != "" {
		c.Visibility = visibility
	} else {
		c.Visibility = visibilityDefault
	}
	return c, nil
}
