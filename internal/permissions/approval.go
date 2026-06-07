package permissions

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

// ApprovalStore defines the interface for approval persistence
type ApprovalStore interface {
	Create(approval *ApprovalRequest) error
	Update(approval *ApprovalRequest) error
	GetByID(id string) (*ApprovalRequest, error)
	ListByRun(runID string) ([]*ApprovalRequest, error)
	ListPending() ([]*ApprovalRequest, error)
}

// ApprovalRequest represents a pending approval
type ApprovalRequest struct {
	ID         string                 `json:"id"`
	RunID      string                 `json:"run_id"`
	StepID     *string                `json:"step_id,omitempty"`
	Capability string                 `json:"capability"`
	Context    map[string]interface{} `json:"context,omitempty"`
	Status     ApprovalStatus         `json:"status"`
	ResolvedAt *time.Time             `json:"resolved_at,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
}

// ApprovalStatus represents the status of an approval request
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
)

// Manager handles approval requests and resolution
type Manager struct {
	store     ApprovalStore
	publisher events.EventPublisher
	mu        sync.RWMutex
	// Channels for signaling approval resolution
	resolvers map[string]chan ApprovalStatus
}

// NewApprovalManager creates a new approval manager
func NewApprovalManager(store ApprovalStore, publisher events.EventPublisher) *Manager {
	return &Manager{
		store:     store,
		publisher: publisher,
		resolvers: make(map[string]chan ApprovalStatus),
	}
}

// RequestApproval creates a new approval request and waits for resolution
func (m *Manager) RequestApproval(ctx context.Context, runID string, stepID *string, capability string, contextData map[string]interface{}) (*ApprovalRequest, error) {
	approval := &ApprovalRequest{
		ID:         uuid.New().String(),
		RunID:      runID,
		StepID:     stepID,
		Capability: capability,
		Context:    contextData,
		Status:     ApprovalPending,
		CreatedAt:  time.Now().UTC(),
	}

	// Persist approval request
	if err := m.store.Create(approval); err != nil {
		return nil, fmt.Errorf("failed to create approval request: %w", err)
	}

	// Create resolver channel
	resolver := make(chan ApprovalStatus, 1)
	m.mu.Lock()
	m.resolvers[approval.ID] = resolver
	m.mu.Unlock()

	// Publish approval requested event
	_, err := m.publisher.Publish(ctx, domain.EventApprovalRequested, runID, stepID, map[string]interface{}{
		"approval_id": approval.ID,
		"capability":  capability,
		"context":     contextData,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to publish approval event: %w", err)
	}

	return approval, nil
}

// Resolve resolves an approval request
func (m *Manager) Resolve(ctx context.Context, approvalID string, approved bool) error {
	approval, err := m.store.GetByID(approvalID)
	if err != nil {
		return fmt.Errorf("approval not found: %w", err)
	}

	if approval.Status != ApprovalPending {
		return fmt.Errorf("approval already resolved: %s", approval.Status)
	}

	// Update approval status
	now := time.Now().UTC()
	approval.ResolvedAt = &now
	if approved {
		approval.Status = ApprovalApproved
	} else {
		approval.Status = ApprovalDenied
	}

	if err := m.store.Update(approval); err != nil {
		return fmt.Errorf("failed to update approval: %w", err)
	}

	// Signal resolver. We take the write lock so Resolve can both deliver the
	// status AND remove the entry atomically; WaitForResolution won't then
	// race with us on the map deletion.
	m.mu.Lock()
	resolver, exists := m.resolvers[approvalID]
	if exists {
		delete(m.resolvers, approvalID)
	}
	m.mu.Unlock()

	if exists {
		// The channel has capacity 1 and we're the sole sender, so this
		// never blocks. If WaitForResolution has already left due to ctx
		// cancel the buffered value is simply discarded when the channel
		// gets garbage-collected.
		resolver <- approval.Status
		close(resolver)
	}

	// Publish approval resolved event (best effort - don't fail if event bus is busy)
	_, _ = m.publisher.Publish(ctx, domain.EventApprovalResolved, approval.RunID, approval.StepID, map[string]interface{}{
		"approval_id": approval.ID,
		"status":      approval.Status,
		"capability":  approval.Capability,
	})

	return nil
}

// WaitForResolution blocks until an approval is resolved or ctx is cancelled.
// On ctx cancellation we remove the resolver entry ourselves so a later
// Resolve call doesn't leak a channel by writing to a map nobody will read.
func (m *Manager) WaitForResolution(ctx context.Context, approvalID string) (ApprovalStatus, error) {
	m.mu.RLock()
	resolver, exists := m.resolvers[approvalID]
	m.mu.RUnlock()

	if !exists {
		// Resolve may already have fired and removed the entry; check
		// persisted state before reporting an error.
		approval, err := m.store.GetByID(approvalID)
		if err != nil {
			return "", fmt.Errorf("approval not found: %w", err)
		}
		if approval.Status != ApprovalPending {
			return approval.Status, nil
		}
		return "", fmt.Errorf("approval resolver not found")
	}

	select {
	case status := <-resolver:
		// Resolve already closed + removed the entry; nothing to clean up.
		return status, nil
	case <-ctx.Done():
		// Caller abandoned the wait. Evict the resolver so Resolve doesn't
		// later publish into a dead channel (and keeps the map bounded).
		m.mu.Lock()
		if cur, ok := m.resolvers[approvalID]; ok && cur == resolver {
			delete(m.resolvers, approvalID)
		}
		m.mu.Unlock()
		return "", ctx.Err()
	}
}

// GetPending returns all pending approvals
func (m *Manager) GetPending() ([]*ApprovalRequest, error) {
	return m.store.ListPending()
}

// GetByRun returns all approvals for a run
func (m *Manager) GetByRun(runID string) ([]*ApprovalRequest, error) {
	return m.store.ListByRun(runID)
}

// GetByID returns a single approval by ID
func (m *Manager) GetByID(id string) (*ApprovalRequest, error) {
	return m.store.GetByID(id)
}

// Cleanup removes old resolver channels
func (m *Manager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, ch := range m.resolvers {
		select {
		case <-ch:
			// Already resolved
			delete(m.resolvers, id)
		default:
			// Still pending
		}
	}
}
