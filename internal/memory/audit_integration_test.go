package memory

import (
	"context"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memstore"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// TestAuditChain_MemoryOps verifies that EmbeddedClient.WithEventBus
// emits memory.store / memory.forget / memory.tombstone audit events
// with content_hash populated. This is the integration backstop for
// ADR 0004 acceptance criterion 5.
func TestAuditChain_MemoryOps(t *testing.T) {
	c, database, _, cleanup := newTestClient(t)
	defer cleanup()

	eventStore := db.NewEventRepository(database)
	bus := events.NewEventBus(eventStore)
	c.WithEventBus(bus)

	sub := bus.Subscribe(events.EventFilter{
		EventTypes: []domain.EventType{
			domain.EventMemoryStore,
			domain.EventMemoryForget,
			domain.EventMemoryTombstone,
		},
	})
	defer sub.Unsubscribe()

	ctx := context.Background()
	scope := memstore.LocalWorkspace()

	// 1. Store → memory.store event with content_hash.
	entry := &memstore.Entry{Content: "audit me"}
	if err := c.Store(ctx, scope, entry); err != nil {
		t.Fatal(err)
	}
	ev := waitForEvent(t, sub, 500*time.Millisecond)
	if ev.Type != domain.EventMemoryStore {
		t.Errorf("event type = %s, want %s", ev.Type, domain.EventMemoryStore)
	}
	if h, _ := ev.Payload["content_hash"].(string); h == "" {
		t.Error("memory.store event missing content_hash")
	} else if h != entry.ContentHash {
		t.Errorf("event content_hash = %q, want %q", h, entry.ContentHash)
	}

	// 2. Forget → memory.forget event.
	if err := c.Forget(ctx, scope, entry.ID); err != nil {
		t.Fatal(err)
	}
	ev = waitForEvent(t, sub, 500*time.Millisecond)
	if ev.Type != domain.EventMemoryForget {
		t.Errorf("event type = %s, want %s", ev.Type, domain.EventMemoryForget)
	}

	// 3. Tombstone → memory.tombstone event.
	if err := c.Tombstone(ctx, memstore.EntityRef{Kind: memstore.EntityAssistant, ID: "missing"}); err != nil {
		t.Fatal(err)
	}
	ev = waitForEvent(t, sub, 500*time.Millisecond)
	if ev.Type != domain.EventMemoryTombstone {
		t.Errorf("event type = %s, want %s", ev.Type, domain.EventMemoryTombstone)
	}
	if k, _ := ev.Payload["entity_kind"].(string); k != string(memstore.EntityAssistant) {
		t.Errorf("tombstone event entity_kind = %q", k)
	}
}

func waitForEvent(t *testing.T, sub events.EventSubscription, d time.Duration) *domain.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(d):
		t.Fatal("timeout waiting for event")
		return nil
	}
}
