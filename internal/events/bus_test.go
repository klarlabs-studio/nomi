package events

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// memoryStore is a minimal in-memory EventStore implementation so tests
// don't need to spin up SQLite for the bus-only scenarios below.
type memoryStore struct {
	mu     sync.Mutex
	events []*domain.Event
	// createErr, if non-nil, is returned from Create to exercise the
	// persistence-failure branch of Publish.
	createErr error
}

func (s *memoryStore) Create(event *domain.Event) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *memoryStore) ListByRun(runID string, _ int) ([]*domain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.Event
	for _, e := range s.events {
		if e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memoryStore) ListAll(limit int) ([]*domain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*domain.Event, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func TestPublishPersistsAndBroadcasts(t *testing.T) {
	store := &memoryStore{}
	bus := NewEventBus(store)
	ctx := context.Background()

	sub := bus.Subscribe(EventFilter{})
	defer sub.Unsubscribe()

	ev, err := bus.Publish(ctx, domain.EventRunCreated, "run-1", nil, map[string]interface{}{"goal": "g"})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if ev.ID == "" {
		t.Fatal("expected generated event ID")
	}
	if len(store.events) != 1 {
		t.Fatalf("expected event persisted, got %d", len(store.events))
	}

	select {
	case got := <-sub.Events():
		if got.RunID != "run-1" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected event on subscriber channel")
	}
}

func TestFilterByRunID(t *testing.T) {
	bus := NewEventBus(&memoryStore{})
	ctx := context.Background()

	runID := "wanted"
	sub := bus.Subscribe(EventFilter{RunID: &runID})
	defer sub.Unsubscribe()

	_, _ = bus.Publish(ctx, domain.EventRunCreated, "other", nil, nil)
	_, _ = bus.Publish(ctx, domain.EventRunCreated, "wanted", nil, nil)

	select {
	case got := <-sub.Events():
		if got.RunID != "wanted" {
			t.Fatalf("filter leaked other run: %+v", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected the wanted event")
	}

	// The other run's event must not be queued.
	select {
	case stray := <-sub.Events():
		t.Fatalf("unexpected event on filtered sub: %+v", stray)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFilterByEventType(t *testing.T) {
	bus := NewEventBus(&memoryStore{})
	ctx := context.Background()

	sub := bus.Subscribe(EventFilter{EventTypes: []domain.EventType{domain.EventStepCompleted}})
	defer sub.Unsubscribe()

	_, _ = bus.Publish(ctx, domain.EventRunCreated, "r", nil, nil)
	_, _ = bus.Publish(ctx, domain.EventStepCompleted, "r", nil, nil)

	select {
	case got := <-sub.Events():
		if got.Type != domain.EventStepCompleted {
			t.Fatalf("got unexpected type: %s", got.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected step.completed event")
	}
}

func TestPublishSurfacesStoreError(t *testing.T) {
	bus := NewEventBus(&memoryStore{createErr: errors.New("disk full")})
	_, err := bus.Publish(context.Background(), domain.EventRunCreated, "r", nil, nil)
	if err == nil {
		t.Fatal("expected persistence error")
	}
}

func TestSlowSubscriberEviction(t *testing.T) {
	bus := NewEventBus(&memoryStore{})
	ctx := context.Background()

	// Subscribe but never drain — every send after the 100-capacity buffer
	// fills will miss, incrementing droppedCount.
	_ = bus.Subscribe(EventFilter{})

	// Buffer capacity is 100 (see subscription setup) and the eviction
	// threshold is slowSubscriberThreshold (50). Publish enough to fill the
	// buffer and then exceed the drop threshold by a clear margin.
	for i := 0; i < 200; i++ {
		_, _ = bus.Publish(ctx, domain.EventRunCreated, "r", nil, nil)
	}

	// Eviction is synchronous inside broadcast, so by the time Publish
	// returns the subscriber should already be removed.
	bus.mu.RLock()
	count := len(bus.subscriptions)
	bus.mu.RUnlock()
	if count != 0 {
		t.Fatalf("expected slow subscriber to be evicted; still have %d subscriptions", count)
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	bus := NewEventBus(&memoryStore{})
	sub := bus.Subscribe(EventFilter{})
	sub.Unsubscribe()
	// Second call should be a no-op rather than panicking on close of a
	// closed channel.
	sub.Unsubscribe()
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	bus := NewEventBus(&memoryStore{})
	ctx := context.Background()

	const publishers = 4
	const perPublisher = 25
	const subscribers = 3

	var received int32
	var wgSubs sync.WaitGroup
	wgSubs.Add(subscribers)
	for i := 0; i < subscribers; i++ {
		go func() {
			defer wgSubs.Done()
			sub := bus.Subscribe(EventFilter{})
			defer sub.Unsubscribe()
			// Drain for a bounded time window; exit when no event for 100ms.
			timer := time.NewTimer(200 * time.Millisecond)
			for {
				select {
				case <-sub.Events():
					atomic.AddInt32(&received, 1)
					timer.Reset(100 * time.Millisecond)
				case <-timer.C:
					return
				}
			}
		}()
	}

	// Give subscribers time to attach before we publish.
	time.Sleep(20 * time.Millisecond)

	var wgPub sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wgPub.Add(1)
		go func() {
			defer wgPub.Done()
			for i := 0; i < perPublisher; i++ {
				_, _ = bus.Publish(ctx, domain.EventRunCreated, "r", nil, nil)
			}
		}()
	}
	wgPub.Wait()
	wgSubs.Wait()

	// At least one subscriber should have seen events — we don't assert an
	// exact count because scheduling variance can cause drops, but zero
	// would indicate a wiring bug.
	if got := atomic.LoadInt32(&received); got == 0 {
		t.Fatal("no subscriber received any event under concurrency")
	}
}

func TestMatchesOnEmptyFilter(t *testing.T) {
	var f EventFilter
	evt := &domain.Event{Type: domain.EventRunCreated, RunID: "r"}
	if !f.Matches(evt) {
		t.Fatal("empty filter should match every event")
	}
}
