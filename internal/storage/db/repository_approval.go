package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/permissions"
)

// ApprovalRepository handles database operations for Approvals
type ApprovalRepository struct {
	db *DB
}

// NewApprovalRepository creates a new ApprovalRepository
func NewApprovalRepository(db *DB) *ApprovalRepository {
	return &ApprovalRepository{db: db}
}

// Create inserts a new approval request
func (r *ApprovalRepository) Create(approval *permissions.ApprovalRequest) error {
	context, err := json.Marshal(approval.Context)
	if err != nil {
		return fmt.Errorf("failed to marshal context: %w", err)
	}

	query := `
		INSERT INTO approvals (id, run_id, step_id, capability, context, status, resolved_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = r.db.Exec(query,
		approval.ID, approval.RunID, approval.StepID, approval.Capability,
		context, approval.Status, approval.ResolvedAt, approval.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create approval: %w", err)
	}
	return nil
}

// Update updates an approval request
func (r *ApprovalRepository) Update(approval *permissions.ApprovalRequest) error {
	query := `
		UPDATE approvals
		SET status = ?, resolved_at = ?
		WHERE id = ?
	`
	_, err := r.db.Exec(query, approval.Status, approval.ResolvedAt, approval.ID)
	if err != nil {
		return fmt.Errorf("failed to update approval: %w", err)
	}
	return nil
}

// GetByID retrieves an approval by ID
func (r *ApprovalRepository) GetByID(id string) (*permissions.ApprovalRequest, error) {
	query := `
		SELECT id, run_id, step_id, capability, context, status, resolved_at, created_at
		FROM approvals WHERE id = ?
	`
	approval := &permissions.ApprovalRequest{}
	var stepID sql.NullString
	var contextData []byte
	var resolvedAt sql.NullTime

	err := r.db.QueryRow(query, id).Scan(
		&approval.ID, &approval.RunID, &stepID, &approval.Capability,
		&contextData, &approval.Status, &resolvedAt, &approval.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("approval not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get approval: %w", err)
	}

	if stepID.Valid {
		approval.StepID = &stepID.String
	}
	if resolvedAt.Valid {
		approval.ResolvedAt = &resolvedAt.Time
	}
	if err := json.Unmarshal(contextData, &approval.Context); err != nil {
		return nil, fmt.Errorf("failed to unmarshal context: %w", err)
	}

	return approval, nil
}

// ListByRun retrieves approvals for a specific run
func (r *ApprovalRepository) ListByRun(runID string) ([]*permissions.ApprovalRequest, error) {
	query := `
		SELECT id, run_id, step_id, capability, context, status, resolved_at, created_at
		FROM approvals WHERE run_id = ? ORDER BY created_at DESC
	`
	rows, err := r.db.Query(query, runID)
	if err != nil {
		return nil, fmt.Errorf("failed to list approvals: %w", err)
	}
	defer rows.Close()

	return r.scanApprovals(rows)
}

// ListPending retrieves all pending approvals
func (r *ApprovalRepository) ListPending() ([]*permissions.ApprovalRequest, error) {
	query := `
		SELECT id, run_id, step_id, capability, context, status, resolved_at, created_at
		FROM approvals WHERE status = ? ORDER BY created_at ASC
	`
	rows, err := r.db.Query(query, permissions.ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending approvals: %w", err)
	}
	defer rows.Close()

	return r.scanApprovals(rows)
}

// ListByTimeRange retrieves approvals in [from, to] by created_at.
func (r *ApprovalRepository) ListByTimeRange(from, to time.Time) ([]*permissions.ApprovalRequest, error) {
	query := `
		SELECT id, run_id, step_id, capability, context, status, resolved_at, created_at
		FROM approvals
		WHERE created_at >= ? AND created_at <= ?
		ORDER BY created_at ASC
	`
	rows, err := r.db.Query(query, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to list approvals by time range: %w", err)
	}
	defer rows.Close()

	return r.scanApprovals(rows)
}

// scanApprovals scans rows into ApprovalRequest structs
func (r *ApprovalRepository) scanApprovals(rows *sql.Rows) ([]*permissions.ApprovalRequest, error) {
	approvals := make([]*permissions.ApprovalRequest, 0)

	for rows.Next() {
		approval := &permissions.ApprovalRequest{}
		var stepID sql.NullString
		var contextData []byte
		var resolvedAt sql.NullTime

		err := rows.Scan(
			&approval.ID, &approval.RunID, &stepID, &approval.Capability,
			&contextData, &approval.Status, &resolvedAt, &approval.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan approval: %w", err)
		}

		if stepID.Valid {
			approval.StepID = &stepID.String
		}
		if resolvedAt.Valid {
			approval.ResolvedAt = &resolvedAt.Time
		}
		if err := json.Unmarshal(contextData, &approval.Context); err != nil {
			return nil, fmt.Errorf("failed to unmarshal context: %w", err)
		}

		approvals = append(approvals, approval)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return approvals, nil
}

// Delete removes an approval
func (r *ApprovalRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM approvals WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete approval: %w", err)
	}
	return nil
}
