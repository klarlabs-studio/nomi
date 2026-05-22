package events

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
)

// EventPublisher defines the interface for publishing events
type EventPublisher interface {
	Publish(ctx context.Context, eventType domain.EventType, runID string, stepID *string, payload map[string]interface{}) (*domain.Event, error)
}

// EventSubscriber defines the interface for subscribing to events
type EventSubscriber interface {
	Subscribe(filter EventFilter) EventSubscription
}

// EventFilter defines criteria for filtering events
type EventFilter struct {
	EventTypes []domain.EventType
	RunID      *string
}

// Matches checks if an event matches the filter
func (f EventFilter) Matches(event *domain.Event) bool {
	if f.RunID != nil && event.RunID != *f.RunID {
		return false
	}

	if len(f.EventTypes) > 0 {
		matched := false
		for _, et := range f.EventTypes {
			if et == event.Type {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// EventSubscription represents an active subscription
type EventSubscription interface {
	Events() <-chan *domain.Event
	Unsubscribe()
}

// subscription implements EventSubscription. droppedCount tracks how many
// events have been skipped because the subscriber's channel was full; after
// slowSubscriberThreshold misses, the bus evicts the subscription so a stuck
// client never permanently wedges delivery. It's accessed via atomic ops
// because broadcast runs under bus.mu.RLock() — multiple goroutines may
// concurrently update the counter for the same subscription.
type subscription struct {
	filter       EventFilter
	events       chan *domain.Event
	bus          *EventBus
	cancel       context.CancelFunc
	mu           sync.RWMutex
	closed       bool
	droppedCount atomic.Uint32
}

// slowSubscriberThreshold is the number of consecutive dropped events we
// tolerate before evicting a subscription. At 100-buffer + 2s polling on the
// UI this effectively means "the client has been unreachable for multiple
// minutes."
const slowSubscriberThreshold = 50

// Events returns the event channel
func (s *subscription) Events() <-chan *domain.Event {
	return s.events
}

// Unsubscribe closes the subscription
func (s *subscription) Unsubscribe() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	s.closed = true
	s.cancel()
	s.bus.removeSubscription(s)
	close(s.events)
}

// EventStore defines the interface for event persistence
type EventStore interface {
	Create(event *domain.Event) error
	ListByRun(runID string, limit int) ([]*domain.Event, error)
	ListAll(limit int) ([]*domain.Event, error)
}

// EventBus implements both EventPublisher and EventSubscriber
type EventBus struct {
	store         EventStore
	subscriptions []*subscription
	mu            sync.RWMutex
}

// NewEventBus creates a new EventBus
func NewEventBus(store EventStore) *EventBus {
	return &EventBus{
		store:         store,
		subscriptions: make([]*subscription, 0),
	}
}

// Publish creates and publishes an event
func (b *EventBus) Publish(ctx context.Context, eventType domain.EventType, runID string, stepID *string, payload map[string]interface{}) (*domain.Event, error) {
	event := &domain.Event{
		ID:        uuid.New().String(),
		Type:      eventType,
		RunID:     runID,
		StepID:    stepID,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}

	// Validate event
	if err := validateEvent(event); err != nil {
		return nil, fmt.Errorf("invalid event: %w", err)
	}

	// Persist event
	if err := b.store.Create(event); err != nil {
		return nil, fmt.Errorf("failed to persist event: %w", err)
	}

	// Broadcast to subscribers
	b.broadcast(event)

	return event, nil
}

// Subscribe creates a new event subscription. The caller is responsible for
// invoking Unsubscribe (typically via defer in the SSE handler). The cancel
// func is retained so future features like bus.Shutdown() can tear every
// subscription down in one pass; today only Unsubscribe uses it.
func (b *EventBus) Subscribe(filter EventFilter) EventSubscription {
	_, cancel := context.WithCancel(context.Background())

	sub := &subscription{
		filter: filter,
		events: make(chan *domain.Event, 100),
		bus:    b,
		cancel: cancel,
	}

	b.mu.Lock()
	b.subscriptions = append(b.subscriptions, sub)
	b.mu.Unlock()

	return sub
}

// removeSubscription removes a subscription from the bus
func (b *EventBus) removeSubscription(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, s := range b.subscriptions {
		if s == sub {
			b.subscriptions = append(b.subscriptions[:i], b.subscriptions[i+1:]...)
			break
		}
	}
}

// Broadcast fans an already-persisted event out to all matching subscribers
// without inserting a new row. Used by callers (typically the runtime) that
// wrote the event inside their own transaction and want the SSE stream
// updated after the commit succeeds.
func (b *EventBus) Broadcast(event *domain.Event) {
	b.broadcast(event)
}

// broadcast sends an event to every matching subscriber. Subscribers whose
// channels are full have a drop counter incremented; once the counter exceeds
// slowSubscriberThreshold the subscription is evicted so a single stuck
// client can't accumulate memory or starve the publisher.
func (b *EventBus) broadcast(event *domain.Event) {
	b.mu.RLock()
	subs := make([]*subscription, len(b.subscriptions))
	copy(subs, b.subscriptions)
	b.mu.RUnlock()

	var toEvict []*subscription
	for _, sub := range subs {
		if !sub.filter.Matches(event) {
			continue
		}
		select {
		case sub.events <- event:
			sub.droppedCount.Store(0)
		default:
			if sub.droppedCount.Add(1) > slowSubscriberThreshold {
				toEvict = append(toEvict, sub)
			}
		}
	}

	for _, sub := range toEvict {
		log.Printf("events: evicting slow subscriber (%d events dropped)", sub.droppedCount.Load())
		sub.Unsubscribe()
	}
}

// GetHistory retrieves event history for a run
func (b *EventBus) GetHistory(runID string, limit int) ([]*domain.Event, error) {
	return b.store.ListByRun(runID, limit)
}

// GetAllHistory retrieves all events
func (b *EventBus) GetAllHistory(limit int) ([]*domain.Event, error) {
	return b.store.ListAll(limit)
}

// validateEvent validates an event before publishing
func validateEvent(event *domain.Event) error {
	if event.ID == "" {
		return fmt.Errorf("event ID is required")
	}
	if event.Type == "" {
		return fmt.Errorf("event type is required")
	}
	if event.RunID == "" && !isEntityScopedEvent(event.Type) {
		return fmt.Errorf("run ID is required")
	}
	if event.Timestamp.IsZero() {
		return fmt.Errorf("timestamp is required")
	}
	return nil
}

// isEntityScopedEvent reports whether the event type targets an entity
// other than a run (e.g. an assistant deletion, a memory operation).
// Such events carry the entity ID in the payload; RunID is left empty.
// Subscribers filtering by RunID will not see these — they're consumed
// by global subscribers (e.g. the runtime's Tombstone wiring).
func isEntityScopedEvent(t domain.EventType) bool {
	switch t {
	case domain.EventAssistantDeleted,
		domain.EventRunDeleted,
		domain.EventMemoryStore,
		domain.EventMemoryForget,
		domain.EventMemoryTombstone:
		return true
	}
	return false
}
