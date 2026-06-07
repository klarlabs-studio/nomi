package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.klarlabs.de/nomi/internal/domain"
)

// RunAttachmentRepository handles CRUD for run_attachments.
type RunAttachmentRepository struct {
	db *DB
}

// NewRunAttachmentRepository constructs a repository.
func NewRunAttachmentRepository(db *DB) *RunAttachmentRepository {
	return &RunAttachmentRepository{db: db}
}

// Create inserts a new attachment. ID is auto-generated when empty so
// channel plugins don't need to mint UUIDs themselves. CreatedAt is
// stamped server-side when zero.
func (r *RunAttachmentRepository) Create(att *domain.RunAttachment) error {
	if att.RunID == "" {
		return fmt.Errorf("run_attachment: run_id is required")
	}
	if att.Kind == "" {
		return fmt.Errorf("run_attachment: kind is required")
	}
	if att.ID == "" {
		att.ID = uuid.New().String()
	}
	if att.CreatedAt.IsZero() {
		att.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(
		`INSERT INTO run_attachments (id, run_id, kind, filename, content_type, url, external_id, size_bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		att.ID, att.RunID, att.Kind, att.Filename, att.ContentType,
		att.URL, att.ExternalID, att.SizeBytes, att.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert run_attachment: %w", err)
	}
	return nil
}

// ListByRun returns every attachment captured for a run. Used by the
// enrichment pass and by the UI to render attachment chips.
func (r *RunAttachmentRepository) ListByRun(runID string) ([]*domain.RunAttachment, error) {
	rows, err := r.db.Query(
		`SELECT id, run_id, kind, filename, content_type, url, external_id, size_bytes, created_at
		 FROM run_attachments WHERE run_id = ? ORDER BY created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.RunAttachment
	for rows.Next() {
		var a domain.RunAttachment
		if err := rows.Scan(&a.ID, &a.RunID, &a.Kind, &a.Filename, &a.ContentType,
			&a.URL, &a.ExternalID, &a.SizeBytes, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// Delete removes one attachment row.
func (r *RunAttachmentRepository) Delete(id string) error {
	_, err := r.db.Exec(`DELETE FROM run_attachments WHERE id = ?`, id)
	return err
}

// CreateBatch is a convenience for plugins that capture multiple
// attachments in a single inbound message. Inserts in one transaction
// so a partial failure doesn't strand the run with half its
// attachments missing.
func (r *RunAttachmentRepository) CreateBatch(atts []*domain.RunAttachment) error {
	if len(atts) == 0 {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, att := range atts {
		if att.ID == "" {
			att.ID = uuid.New().String()
		}
		if att.CreatedAt.IsZero() {
			att.CreatedAt = time.Now().UTC()
		}
		if _, err := tx.Exec(
			`INSERT INTO run_attachments (id, run_id, kind, filename, content_type, url, external_id, size_bytes, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			att.ID, att.RunID, att.Kind, att.Filename, att.ContentType,
			att.URL, att.ExternalID, att.SizeBytes, att.CreatedAt,
		); err != nil {
			return fmt.Errorf("batch insert: %w", err)
		}
	}
	return tx.Commit()
}

// Compile-time guard that sql is used; needed once the package grows
// helpers that don't directly reference database/sql.
var _ = sql.ErrNoRows
