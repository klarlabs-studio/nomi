package permissions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// memoryApprovalStore is a lightweight in-memory ApprovalStore for
// coverage-focused unit tests. Production code uses the SQLite-backed repo.
type memoryApprovalStore struct {
	mu   sync.Mutex
	data map[string]*ApprovalRequest
}

func newMemoryApprovalStore() *memoryApprovalStore {
	return &memoryApprovalStore{data: make(map[string]*ApprovalRequest)}
}

func (s *memoryApprovalStore) Create(a *ApprovalRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[a.ID] = a
	return nil
}

func (s *memoryApprovalStore) Update(a *ApprovalRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[a.ID] = a
	return nil
}

func (s *memoryApprovalStore) GetByID(id string) (*ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.data[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return a, nil
}

func (s *memoryApprovalStore) ListByRun(runID string) ([]*ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ApprovalRequest
	for _, a := range s.data {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *memoryApprovalStore) ListPending() ([]*ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ApprovalRequest
	for _, a := range s.data {
		if a.Status == ApprovalPending {
			out = append(out, a)
		}
	}
	return out, nil
}

// fakePublisher records event publications without fanning out.
type fakePublisher struct {
	mu     sync.Mutex
	events []domain.EventType
}

func (p *fakePublisher) Publish(_ context.Context, t domain.EventType, _ string, _ *string, _ map[string]interface{}) (*domain.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, t)
	return &domain.Event{Type: t}, nil
}

func newTestManager() (*Manager, *memoryApprovalStore, *fakePublisher) {
	store := newMemoryApprovalStore()
	pub := &fakePublisher{}
	return NewApprovalManager(store, pub), store, pub
}

func TestRequestApprovalPersistsAndPublishes(t *testing.T) {
	mgr, store, pub := newTestManager()
	req, err := mgr.RequestApproval(context.Background(), "run-1", nil, "filesystem.write", map[string]interface{}{"path": "/tmp"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if req.Status != ApprovalPending {
		t.Fatalf("status = %s, want pending", req.Status)
	}
	if _, err := store.GetByID(req.ID); err != nil {
		t.Fatalf("not persisted: %v", err)
	}
	if len(pub.events) != 1 || pub.events[0] != domain.EventApprovalRequested {
		t.Fatalf("expected one approval.requested event, got %v", pub.events)
	}
}

func TestResolveApproved(t *testing.T) {
	mgr, _, pub := newTestManager()
	req, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "cmd", nil)

	done := make(chan ApprovalStatus, 1)
	go func() {
		status, err := mgr.WaitForResolution(context.Background(), req.ID)
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		done <- status
	}()

	// Let the waiter park on the resolver channel before resolving.
	time.Sleep(10 * time.Millisecond)
	if err := mgr.Resolve(context.Background(), req.ID, true); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	select {
	case got := <-done:
		if got != ApprovalApproved {
			t.Fatalf("waiter saw %s", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitForResolution did not return after Resolve")
	}
	// Exactly one approval.requested + one approval.resolved should have published.
	if len(pub.events) != 2 {
		t.Fatalf("expected 2 events, got %v", pub.events)
	}
}

func TestResolveDenied(t *testing.T) {
	mgr, _, _ := newTestManager()
	req, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "cmd", nil)

	if err := mgr.Resolve(context.Background(), req.ID, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// After resolve, a later Wait finds the persisted terminal state.
	status, err := mgr.WaitForResolution(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("post-resolve wait: %v", err)
	}
	if status != ApprovalDenied {
		t.Fatalf("status = %s", status)
	}
}

func TestResolveAlreadyResolvedIsRejected(t *testing.T) {
	mgr, _, _ := newTestManager()
	req, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "cmd", nil)
	_ = mgr.Resolve(context.Background(), req.ID, true)
	if err := mgr.Resolve(context.Background(), req.ID, false); err == nil {
		t.Fatal("expected error on double-resolve")
	}
}

func TestWaitForResolutionContextCancelEvictsEntry(t *testing.T) {
	mgr, _, _ := newTestManager()
	req, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "cmd", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we call Wait

	if _, err := mgr.WaitForResolution(ctx, req.ID); err == nil {
		t.Fatal("expected ctx.Err from cancelled wait")
	}

	// After the aborted wait, the resolver map must have been cleaned up so
	// a subsequent Resolve doesn't write into a dead channel.
	mgr.mu.Lock()
	_, stillThere := mgr.resolvers[req.ID]
	mgr.mu.Unlock()
	if stillThere {
		t.Fatal("resolver entry leaked after ctx cancel")
	}

	// Resolve still succeeds (updates persisted state).
	if err := mgr.Resolve(context.Background(), req.ID, true); err != nil {
		t.Fatalf("resolve after abandoned wait: %v", err)
	}
}

func TestGetPendingAndGetByRun(t *testing.T) {
	mgr, _, _ := newTestManager()
	r1, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "a", nil)
	r2, _ := mgr.RequestApproval(context.Background(), "run-1", nil, "b", nil)
	_, _ = mgr.RequestApproval(context.Background(), "run-2", nil, "c", nil)

	pending, err := mgr.GetPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("want 3 pending, got %d", len(pending))
	}

	byRun, err := mgr.GetByRun("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(byRun) != 2 {
		t.Fatalf("want 2 for run-1, got %d", len(byRun))
	}
	_ = r1
	_ = r2

	one, err := mgr.GetByID(r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if one.ID != r1.ID {
		t.Fatalf("wrong id")
	}
}

func TestValidatePolicyDetectsDuplicates(t *testing.T) {
	e := NewEngine()
	err := e.ValidatePolicy(domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.read", Mode: domain.PermissionAllow},
		{Capability: "filesystem.read", Mode: domain.PermissionDeny},
	}})
	if err == nil {
		t.Fatal("expected duplicate-capability error")
	}
}

func TestValidatePolicyRejectsEmptyCapability(t *testing.T) {
	e := NewEngine()
	err := e.ValidatePolicy(domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "", Mode: domain.PermissionAllow},
	}})
	if err == nil {
		t.Fatal("expected empty-capability error")
	}
}

func TestValidatePolicyRejectsInvalidMode(t *testing.T) {
	e := NewEngine()
	err := e.ValidatePolicy(domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.read", Mode: domain.PermissionMode("weird")},
	}})
	if err == nil {
		t.Fatal("expected invalid-mode error")
	}
}

func TestMergePoliciesLaterWins(t *testing.T) {
	base := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
		{Capability: "filesystem.read", Mode: domain.PermissionAllow},
	}}
	override := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		// Later policies take precedence on duplicate keys.
		{Capability: "filesystem.write", Mode: domain.PermissionDeny},
	}}
	merged := MergePolicies(base, override)

	e := NewEngine()
	if got := e.Evaluate(merged, "filesystem.write"); got != domain.PermissionDeny {
		t.Fatalf("merge should let later policy win: got %s", got)
	}
	if got := e.Evaluate(merged, "filesystem.read"); got != domain.PermissionAllow {
		t.Fatalf("non-overridden rule should pass through: got %s", got)
	}
}

func TestBuildPoliciesContent(t *testing.T) {
	// Quick coverage of the three policy-builder helpers.
	e := NewEngine()
	def := BuildDefaultPolicy()
	if got := e.Evaluate(def, "filesystem.read"); got != domain.PermissionAllow {
		t.Fatalf("default filesystem.read should allow: %s", got)
	}
	if got := e.Evaluate(def, "command.exec"); got != domain.PermissionConfirm {
		t.Fatalf("default command.exec should confirm: %s", got)
	}
	if got := e.Evaluate(def, "network.outgoing"); got != domain.PermissionDeny {
		t.Fatalf("default network.* should deny: %s", got)
	}

	permissive := BuildPermissivePolicy()
	if got := e.Evaluate(permissive, "anything.at.all"); got != domain.PermissionAllow {
		t.Fatalf("permissive policy should allow everything: %s", got)
	}

	restricted := BuildRestrictedPolicy()
	if got := e.Evaluate(restricted, "filesystem.write"); got != domain.PermissionDeny {
		t.Fatalf("restricted filesystem.write should deny: %s", got)
	}
}
