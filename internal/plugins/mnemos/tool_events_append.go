package mnemos

import (
	"context"
	"fmt"

	mnemosclient "go.klarlabs.de/mnemos/client"
)

// eventsAppendTool implements mnemos.events.append. Capability:
// mnemos.write.
//
// Input shape:
//
//	{
//	  "connection_id": "company-mnemos",
//	  "events": [
//	    { "run_id": "...", "content": "...", "source_input_id": "...",
//	      "metadata": {"k":"v"}, "timestamp": "2026-05-22T..." }
//	  ]
//	}
//
// At least one event required. Each event must carry run_id + content;
// source_input_id is optional but recommended. Server fills ID and
// IngestedAt; supplying ID is allowed for idempotency tests.
type eventsAppendTool struct{ p *Plugin }

func (t *eventsAppendTool) Name() string       { return ToolEventsAppend }
func (t *eventsAppendTool) Capability() string { return CapWrite }

func (t *eventsAppendTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connID, _ := input["connection_id"].(string)
	conn, err := t.p.resolveConnection(connID)
	if err != nil {
		return nil, err
	}

	rawEvents, ok := input["events"].([]interface{})
	if !ok || len(rawEvents) == 0 {
		return nil, fmt.Errorf("%s: input.events must be a non-empty array", ToolEventsAppend)
	}

	events := make([]mnemosclient.Event, 0, len(rawEvents))
	for i, re := range rawEvents {
		em, ok := re.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s: events[%d] must be an object", ToolEventsAppend, i)
		}
		ev, err := parseEvent(em)
		if err != nil {
			return nil, fmt.Errorf("%s: events[%d]: %w", ToolEventsAppend, i, err)
		}
		events = append(events, ev)
	}

	resp, err := conn.client.Events().Append(ctx, events)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ToolEventsAppend, err)
	}

	return map[string]interface{}{
		"accepted": resp.Accepted,
		"skipped":  resp.Skipped,
	}, nil
}

// parseEvent validates and converts a single input map into a typed
// Event. Returns the first validation error rather than collecting
// them — the planner gets one failure at a time and can repair.
func parseEvent(m map[string]interface{}) (mnemosclient.Event, error) {
	var ev mnemosclient.Event
	runID, _ := m["run_id"].(string)
	if runID == "" {
		return ev, fmt.Errorf("run_id required")
	}
	content, _ := m["content"].(string)
	if content == "" {
		return ev, fmt.Errorf("content required")
	}
	ev.RunID = runID
	ev.Content = content
	if id, ok := m["id"].(string); ok {
		ev.ID = id
	}
	if sourceID, ok := m["source_input_id"].(string); ok {
		ev.SourceInputID = sourceID
	}
	if schemaVersion, ok := m["schema_version"].(string); ok {
		ev.SchemaVersion = schemaVersion
	}
	if ts, ok := m["timestamp"].(string); ok {
		ev.Timestamp = ts
	}
	if metaRaw, ok := m["metadata"].(map[string]interface{}); ok {
		meta := make(map[string]string, len(metaRaw))
		for k, v := range metaRaw {
			if s, ok := v.(string); ok {
				meta[k] = s
			}
		}
		ev.Metadata = meta
	}
	return ev, nil
}
