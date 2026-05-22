package mnemos

import (
	"context"
	"fmt"

	mnemosclient "github.com/felixgeelhaar/mnemos/client"
)

// embeddingsAppendTool implements mnemos.embeddings.append. Capability:
// mnemos.write.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "embeddings": [
//	    { "entity_id": "...", "entity_type": "claim",
//	      "vector": [0.1, 0.2, ...], "model": "openai/text-embedding-3",
//	      "dimensions": 1536 }
//	  ]
//	}
type embeddingsAppendTool struct{ p *Plugin }

func (t *embeddingsAppendTool) Name() string       { return ToolEmbeddingsAppend }
func (t *embeddingsAppendTool) Capability() string { return CapWrite }

func (t *embeddingsAppendTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	rawEmbeddings, ok := input["embeddings"].([]interface{})
	if !ok || len(rawEmbeddings) == 0 {
		return nil, fmt.Errorf("%s: input.embeddings must be a non-empty array", ToolEmbeddingsAppend)
	}

	embs := make([]mnemosclient.Embedding, 0, len(rawEmbeddings))
	for i, re := range rawEmbeddings {
		em, ok := re.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s: embeddings[%d] must be an object", ToolEmbeddingsAppend, i)
		}
		emb, err := parseEmbedding(em)
		if err != nil {
			return nil, fmt.Errorf("%s: embeddings[%d]: %w", ToolEmbeddingsAppend, i, err)
		}
		embs = append(embs, emb)
	}

	resp, err := conn.client.Embeddings().Append(ctx, embs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolEmbeddingsAppend, err)
	}
	return map[string]interface{}{
		"accepted": resp.Accepted,
		"skipped":  resp.Skipped,
	}, nil
}

// parseEmbedding validates and converts a single input map. Vectors
// arrive as []interface{} of float64 (json.Unmarshal default); convert
// to []float32 since that's the upstream wire shape.
func parseEmbedding(m map[string]interface{}) (mnemosclient.Embedding, error) {
	var e mnemosclient.Embedding
	entityID, _ := m["entity_id"].(string)
	if entityID == "" {
		return e, fmt.Errorf("entity_id required")
	}
	entityType, _ := m["entity_type"].(string)
	if entityType != "event" && entityType != "claim" {
		return e, fmt.Errorf("entity_type must be 'event' or 'claim'")
	}
	rawVec, ok := m["vector"].([]interface{})
	if !ok || len(rawVec) == 0 {
		return e, fmt.Errorf("vector required (non-empty array of numbers)")
	}
	vec := make([]float32, 0, len(rawVec))
	for i, v := range rawVec {
		switch n := v.(type) {
		case float64:
			vec = append(vec, float32(n))
		case float32:
			vec = append(vec, n)
		case int:
			vec = append(vec, float32(n))
		default:
			return e, fmt.Errorf("vector[%d] must be a number", i)
		}
	}
	e.EntityID = entityID
	e.EntityType = entityType
	e.Vector = vec
	if model, ok := m["model"].(string); ok {
		e.Model = model
	}
	if dims, ok := intFromInput(m, "dimensions"); ok {
		e.Dimensions = dims
	}
	return e, nil
}
