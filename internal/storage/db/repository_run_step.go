package db

import (
	"database/sql"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// RunRepository handles database operations for Runs
type RunRepository struct {
	db *DB
}

// NewRunRepository creates a new RunRepository
func NewRunRepository(db *DB) *RunRepository {
	return &RunRepository{db: db}
}

func toNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// Create inserts a new run
func (r *RunRepository) Create(run *domain.Run) error {
	query := `
		INSERT INTO runs (id, goal, assistant_id, source, conversation_id, status, current_step_id, plan_version, run_parent_id, branched_from_step_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.Exec(query,
		run.ID, run.Goal, run.AssistantID, toNullString(run.Source), toNullString(run.ConversationID), run.Status,
		run.CurrentStepID, run.PlanVersion, toNullString(run.RunParentID), toNullString(run.BranchedFromStepID),
		run.CreatedAt, run.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create run: %w", err)
	}
	return nil
}

// GetByID retrieves a run by ID
func (r *RunRepository) GetByID(id string) (*domain.Run, error) {
	query := `
		SELECT id, goal, assistant_id, source, conversation_id, status, current_step_id, plan_version, run_parent_id, branched_from_step_id, created_at, updated_at
		FROM runs WHERE id = ?
	`
	run := &domain.Run{}
	var source, conversationID, currentStepID, runParentID, branchedFromStepID sql.NullString

	err := r.db.QueryRow(query, id).Scan(
		&run.ID, &run.Goal, &run.AssistantID, &source, &conversationID, &run.Status,
		&currentStepID, &run.PlanVersion, &runParentID, &branchedFromStepID, &run.CreatedAt, &run.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("run not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get run: %w", err)
	}

	if source.Valid {
		run.Source = &source.String
	}
	if conversationID.Valid {
		run.ConversationID = &conversationID.String
	}
	if currentStepID.Valid {
		run.CurrentStepID = &currentStepID.String
	}
	if runParentID.Valid {
		run.RunParentID = &runParentID.String
	}
	if branchedFromStepID.Valid {
		run.BranchedFromStepID = &branchedFromStepID.String
	}

	return run, nil
}

// Update updates a run. busy_timeout on the connection handles SQLITE_BUSY
// contention at the driver level; no ad-hoc retry loop needed.
func (r *RunRepository) Update(run *domain.Run) error {
	return r.update(r.db, run)
}

// UpdateTx updates a run inside the caller's transaction.
func (r *RunRepository) UpdateTx(tx *sql.Tx, run *domain.Run) error {
	return r.update(tx, run)
}

// CASUpdateStatusTx performs a compare-and-swap status update inside the
// caller's transaction. Returns sql.ErrNoRows when the WHERE clause
// matched zero rows — meaning a concurrent writer already advanced the
// run past `from`. Callers should treat that as a benign race and skip
// the duplicate transition. Other update fields (current_step_id,
// plan_version, source) are passed through verbatim so the caller's
// in-memory mutation is reflected on disk.
func (r *RunRepository) CASUpdateStatusTx(
	tx *sql.Tx,
	runID string,
	from, to domain.RunStatus,
	currentStepID *string,
	planVersion int,
	source *string,
) error {
	res, err := tx.Exec(
		`UPDATE runs
		   SET status = ?, current_step_id = ?, plan_version = ?, source = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		to, currentStepID, planVersion, toNullString(source),
		time.Now(), runID, from,
	)
	if err != nil {
		return fmt.Errorf("failed to CAS-update run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func (r *RunRepository) update(e execer, run *domain.Run) error {
	query := `
		UPDATE runs
		SET goal = ?, assistant_id = ?, source = ?, conversation_id = ?, status = ?, current_step_id = ?, plan_version = ?, run_parent_id = ?, branched_from_step_id = ?, updated_at = ?
		WHERE id = ?
	`
	if _, err := e.Exec(query,
		run.Goal, run.AssistantID, toNullString(run.Source), toNullString(run.ConversationID), run.Status,
		run.CurrentStepID, run.PlanVersion, toNullString(run.RunParentID), toNullString(run.BranchedFromStepID),
		time.Now(), run.ID,
	); err != nil {
		return fmt.Errorf("failed to update run: %w", err)
	}
	return nil
}

// List retrieves runs with optional status filter
func (r *RunRepository) List(status *domain.RunStatus, limit, offset int) ([]*domain.Run, error) {
	query := `
		SELECT id, goal, assistant_id, source, conversation_id, status, current_step_id, plan_version, run_parent_id, branched_from_step_id, created_at, updated_at
		FROM runs
	`
	var args []interface{}

	if status != nil {
		query += " WHERE status = ?"
		args = append(args, *status)
	}

	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return r.scanRuns(rows)
}

// Search returns runs whose goal OR any owned step's title contains
// the query (case-insensitive). Returns up to `limit` rows ordered by
// created_at DESC. Empty query returns the most recent runs unfiltered
// — same shape as List(nil, limit, 0) — so callers can just always
// call Search and toggle the input.
//
// Implementation uses LIKE %q% on existing indices; no FTS5 dependency.
// Adequate for the chat-list use case where the corpus is "this user's
// runs", typically &lt; 1000 rows.
func (r *RunRepository) Search(query string, limit int) ([]*domain.Run, error) {
	if limit <= 0 {
		limit = 50
	}
	if query == "" {
		return r.List(nil, limit, 0)
	}
	pattern := "%" + query + "%"
	rows, err := r.db.Query(`
		SELECT DISTINCT r.id, r.goal, r.assistant_id, r.source, r.conversation_id, r.status, r.current_step_id, r.plan_version, r.run_parent_id, r.branched_from_step_id, r.created_at, r.updated_at
		FROM runs r
		LEFT JOIN steps s ON s.run_id = r.id
		WHERE LOWER(r.goal) LIKE LOWER(?)
		   OR LOWER(s.title) LIKE LOWER(?)
		ORDER BY r.created_at DESC
		LIMIT ?
	`, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.scanRuns(rows)
}

// ListByStatusIn returns all runs whose status is in the given set. Used by
// the startup resumer to find orphaned runs across any non-terminal state.
func (r *RunRepository) ListByStatusIn(statuses []domain.RunStatus) ([]*domain.Run, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(statuses))
	args := make([]interface{}, len(statuses))
	for i, s := range statuses {
		placeholders[i] = "?"
		args[i] = s
	}
	query := `
		SELECT id, goal, assistant_id, source, conversation_id, status, current_step_id, plan_version, run_parent_id, branched_from_step_id, created_at, updated_at
		FROM runs
		WHERE status IN (` + joinStrings(placeholders, ",") + `)
		ORDER BY created_at ASC
	`
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list runs by status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.scanRuns(rows)
}

// ListChildren returns all runs branched from a given parent run.
func (r *RunRepository) ListChildren(parentID string) ([]*domain.Run, error) {
	query := `
		SELECT id, goal, assistant_id, source, conversation_id, status, current_step_id, plan_version, run_parent_id, branched_from_step_id, created_at, updated_at
		FROM runs WHERE run_parent_id = ? ORDER BY created_at ASC
	`
	rows, err := r.db.Query(query, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to list child runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.scanRuns(rows)
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// scanRuns scans rows into Run structs
func (r *RunRepository) scanRuns(rows *sql.Rows) ([]*domain.Run, error) {
	runs := make([]*domain.Run, 0)

	for rows.Next() {
		run := &domain.Run{}
		var source, conversationID, currentStepID, runParentID, branchedFromStepID sql.NullString

		err := rows.Scan(
			&run.ID, &run.Goal, &run.AssistantID, &source, &conversationID, &run.Status,
			&currentStepID, &run.PlanVersion, &runParentID, &branchedFromStepID, &run.CreatedAt, &run.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan run: %w", err)
		}

		if source.Valid {
			run.Source = &source.String
		}
		if conversationID.Valid {
			run.ConversationID = &conversationID.String
		}
		if currentStepID.Valid {
			run.CurrentStepID = &currentStepID.String
		}
		if runParentID.Valid {
			run.RunParentID = &runParentID.String
		}
		if branchedFromStepID.Valid {
			run.BranchedFromStepID = &branchedFromStepID.String
		}

		runs = append(runs, run)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return runs, nil
}

// Delete removes a run and its associated steps (cascade)
func (r *RunRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM runs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete run: %w", err)
	}
	return nil
}

// StepRepository handles database operations for Steps
type StepRepository struct {
	db *DB
}

// NewStepRepository creates a new StepRepository
func NewStepRepository(db *DB) *StepRepository {
	return &StepRepository{db: db}
}

// Create inserts a new step
func (r *StepRepository) Create(step *domain.Step) error {
	query := `
		INSERT INTO steps (id, run_id, step_definition_id, title, status, input, output, error, retry_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.Exec(query,
		step.ID, step.RunID, step.StepDefinitionID, step.Title, step.Status,
		step.Input, step.Output, step.Error, step.RetryCount,
		step.CreatedAt, step.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create step: %w", err)
	}
	return nil
}

// GetByID retrieves a step by ID
func (r *StepRepository) GetByID(id string) (*domain.Step, error) {
	query := `
		SELECT id, run_id, step_definition_id, title, status, input, output, error, retry_count, created_at, updated_at
		FROM steps WHERE id = ?
	`
	step := &domain.Step{}
	var errorStr, stepDefID sql.NullString

	err := r.db.QueryRow(query, id).Scan(
		&step.ID, &step.RunID, &stepDefID, &step.Title, &step.Status,
		&step.Input, &step.Output, &errorStr, &step.RetryCount,
		&step.CreatedAt, &step.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("step not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get step: %w", err)
	}

	if stepDefID.Valid {
		step.StepDefinitionID = &stepDefID.String
	}
	if errorStr.Valid {
		step.Error = &errorStr.String
	}

	return step, nil
}

// Update updates a step. Driver-level busy_timeout handles contention.
func (r *StepRepository) Update(step *domain.Step) error {
	return r.update(r.db, step)
}

// UpdateTx updates a step inside the caller's transaction.
func (r *StepRepository) UpdateTx(tx *sql.Tx, step *domain.Step) error {
	return r.update(tx, step)
}

func (r *StepRepository) update(e execer, step *domain.Step) error {
	query := `
		UPDATE steps
		SET title = ?, status = ?, input = ?, output = ?, error = ?, retry_count = ?, updated_at = ?
		WHERE id = ?
	`
	if _, err := e.Exec(query,
		step.Title, step.Status, step.Input, step.Output,
		step.Error, step.RetryCount, time.Now(), step.ID,
	); err != nil {
		return fmt.Errorf("failed to update step: %w", err)
	}
	return nil
}

// ListByRun retrieves steps for a specific run
func (r *StepRepository) ListByRun(runID string) ([]*domain.Step, error) {
	query := `
		SELECT id, run_id, step_definition_id, title, status, input, output, error, retry_count, created_at, updated_at
		FROM steps WHERE run_id = ? ORDER BY created_at ASC
	`
	rows, err := r.db.Query(query, runID)
	if err != nil {
		return nil, fmt.Errorf("failed to list steps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return r.scanSteps(rows)
}

// scanSteps scans rows into Step structs
func (r *StepRepository) scanSteps(rows *sql.Rows) ([]*domain.Step, error) {
	steps := make([]*domain.Step, 0)

	for rows.Next() {
		step := &domain.Step{}
		var errorStr, stepDefID sql.NullString

		err := rows.Scan(
			&step.ID, &step.RunID, &stepDefID, &step.Title, &step.Status,
			&step.Input, &step.Output, &errorStr, &step.RetryCount,
			&step.CreatedAt, &step.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan step: %w", err)
		}

		if stepDefID.Valid {
			step.StepDefinitionID = &stepDefID.String
		}
		if errorStr.Valid {
			step.Error = &errorStr.String
		}

		steps = append(steps, step)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return steps, nil
}

// Delete removes a step
func (r *StepRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM steps WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete step: %w", err)
	}
	return nil
}
