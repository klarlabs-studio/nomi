package memory

import (
	"context"

	"github.com/felixgeelhaar/mnemos/embedded"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
)

// BusEmitter adapts an *events.EventBus to embedded.Emitter so memory
// operations land in Nomi's hash-chained audit log. The embedded
// backend calls Emit with its own event-type strings (memory.store /
// memory.forget / memory.tombstone); BusEmitter maps each to the
// corresponding domain.EventType so the SSE stream and verification
// chain see them as first-class events.
//
// Best-effort: Publish failures are dropped. The underlying memory
// operation already succeeded; failing to emit would force a
// false-negative on a write the user can already observe in the
// store.
type BusEmitter struct {
	bus *events.EventBus
}

// NewBusEmitter wires the bus into the adapter. Returns nil if bus is
// nil so callers can chain `embedded.Open(...).WithEmitter(memory.NewBusEmitter(nil))`
// without a guard.
func NewBusEmitter(bus *events.EventBus) embedded.Emitter {
	if bus == nil {
		return nil
	}
	return &BusEmitter{bus: bus}
}

// Emit translates the embedded event string into a domain.EventType
// and publishes through the bus. Unknown event strings are silently
// dropped — the bus's validator would reject them anyway and there's
// no recovery action.
func (e *BusEmitter) Emit(ctx context.Context, eventType string, payload map[string]any) {
	var domainType domain.EventType
	switch eventType {
	case embedded.EventStore:
		domainType = domain.EventMemoryStore
	case embedded.EventForget:
		domainType = domain.EventMemoryForget
	case embedded.EventTombstone:
		domainType = domain.EventMemoryTombstone
	default:
		return
	}
	// map[string]any -> map[string]interface{} is identity in Go but
	// the bus signature wants the alias-named variant.
	p := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		p[k] = v
	}
	_, _ = e.bus.Publish(ctx, domainType, "", nil, p)
}
