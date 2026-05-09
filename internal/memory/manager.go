package memory

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// Store defines the interface for memory operations
type Store interface {
	Save(entry *domain.MemoryEntry) error
	GetByID(id string) (*domain.MemoryEntry, error)
	Search(scope string, query string, limit int) ([]*domain.MemoryEntry, error)
	ListByScope(scope string, limit int) ([]*domain.MemoryEntry, error)
	ListByAssistant(assistantID string, limit int) ([]*domain.MemoryEntry, error)
	Delete(id string) error
}

// Manager implements the Store interface
type Manager struct {
	repo *db.MemoryRepository
}

// NewManager creates a new memory manager
func NewManager(repo *db.MemoryRepository) *Manager {
	return &Manager{repo: repo}
}

// Save stores a memory entry
func (m *Manager) Save(entry *domain.MemoryEntry) error {
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if entry.Scope == "" {
		entry.Scope = "workspace"
	}

	return m.repo.Create(entry)
}

// GetByID retrieves a memory entry by ID
func (m *Manager) GetByID(id string) (*domain.MemoryEntry, error) {
	return m.repo.GetByID(id)
}

// Search searches memory entries by scope and query
func (m *Manager) Search(scope string, query string, limit int) ([]*domain.MemoryEntry, error) {
	if scope != "" {
		// First get by scope, then filter by query
		entries, err := m.repo.ListByScope(scope, limit)
		if err != nil {
			return nil, err
		}

		if query != "" {
			return filterEntries(entries, query), nil
		}
		return entries, nil
	}

	return m.repo.Search(query, limit)
}

// ListByScope lists memory entries by scope
func (m *Manager) ListByScope(scope string, limit int) ([]*domain.MemoryEntry, error) {
	return m.repo.ListByScope(scope, limit)
}

// ListByAssistant lists memory entries for an assistant
func (m *Manager) ListByAssistant(assistantID string, limit int) ([]*domain.MemoryEntry, error) {
	return m.repo.ListByAssistant(assistantID, limit)
}

// Delete removes a memory entry
func (m *Manager) Delete(id string) error {
	return m.repo.Delete(id)
}

// StoreRunMemory stores memory from a run execution
func (m *Manager) StoreRunMemory(ctx context.Context, runID string, assistantID *string, content string) error {
	entry := &domain.MemoryEntry{
		ID:          uuid.New().String(),
		Scope:       "workspace",
		Content:     content,
		AssistantID: assistantID,
		RunID:       &runID,
		CreatedAt:   time.Now().UTC(),
	}

	return m.Save(entry)
}

// StoreProfileMemory stores memory in the profile scope
func (m *Manager) StoreProfileMemory(content string) error {
	entry := &domain.MemoryEntry{
		ID:        uuid.New().String(),
		Scope:     "profile",
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}

	return m.Save(entry)
}

// filterEntries filters entries by a case-insensitive substring
// match against any whitespace-delimited token in the query. An empty
// query returns the input unchanged. Until SQLite FTS5 lands this is
// the cheapest improvement that prevents "Auth" / "auth" mismatches
// hiding the most-relevant rows from the planner-context retrieval.
func filterEntries(entries []*domain.MemoryEntry, query string) []*domain.MemoryEntry {
	if query == "" {
		return entries
	}
	q := strings.ToLower(query)
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return entries
	}
	var filtered []*domain.MemoryEntry
	for _, entry := range entries {
		body := strings.ToLower(entry.Content)
		for _, t := range tokens {
			if strings.Contains(body, t) {
				filtered = append(filtered, entry)
				break
			}
		}
	}
	return filtered
}
