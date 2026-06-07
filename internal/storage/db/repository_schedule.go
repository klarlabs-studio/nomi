package db

import (
	"database/sql"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// ScheduleRepository persists Schedule rows. CRUD is keyed by ID; the
// scheduler ticker uses DueBefore to pick up schedules whose next-fire
// has elapsed.
type ScheduleRepository struct {
	db *DB
}

// NewScheduleRepository constructs a ScheduleRepository.
func NewScheduleRepository(db *DB) *ScheduleRepository {
	return &ScheduleRepository{db: db}
}

// Create inserts a Schedule.
func (r *ScheduleRepository) Create(s *domain.Schedule) error {
	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	_, err := r.db.Exec(`
		INSERT INTO schedules (id, assistant_id, prompt, cron_expr, nl_phrase, enabled, next_fire_at, last_fire_at, last_run_id, last_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		s.ID, s.AssistantID, s.Prompt, s.CronExpr, s.NLPhrase, enabled, s.NextFireAt,
		nullableTime(s.LastFireAt), nullableString(s.LastRunID), nullableString(s.LastError),
		s.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create schedule: %w", err)
	}
	return nil
}

// GetByID fetches a single Schedule.
func (r *ScheduleRepository) GetByID(id string) (*domain.Schedule, error) {
	row := r.db.QueryRow(`
		SELECT id, assistant_id, prompt, cron_expr, nl_phrase, enabled, next_fire_at, last_fire_at, last_run_id, last_error, created_at
		FROM schedules WHERE id = ?
	`, id)
	return scanSchedule(row)
}

// List returns all schedules ordered by created_at desc.
func (r *ScheduleRepository) List() ([]*domain.Schedule, error) {
	rows, err := r.db.Query(`
		SELECT id, assistant_id, prompt, cron_expr, nl_phrase, enabled, next_fire_at, last_fire_at, last_run_id, last_error, created_at
		FROM schedules ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	out := []*domain.Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DueBefore returns enabled schedules whose next_fire_at has elapsed.
// Used by the scheduler ticker to pick the next batch.
func (r *ScheduleRepository) DueBefore(t time.Time) ([]*domain.Schedule, error) {
	rows, err := r.db.Query(`
		SELECT id, assistant_id, prompt, cron_expr, nl_phrase, enabled, next_fire_at, last_fire_at, last_run_id, last_error, created_at
		FROM schedules WHERE enabled = 1 AND next_fire_at <= ?
		ORDER BY next_fire_at ASC
	`, t)
	if err != nil {
		return nil, fmt.Errorf("due schedules: %w", err)
	}
	defer rows.Close()
	out := []*domain.Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Update overwrites every column.
func (r *ScheduleRepository) Update(s *domain.Schedule) error {
	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	_, err := r.db.Exec(`
		UPDATE schedules
		SET assistant_id = ?, prompt = ?, cron_expr = ?, nl_phrase = ?, enabled = ?,
		    next_fire_at = ?, last_fire_at = ?, last_run_id = ?, last_error = ?
		WHERE id = ?
	`,
		s.AssistantID, s.Prompt, s.CronExpr, s.NLPhrase, enabled,
		s.NextFireAt, nullableTime(s.LastFireAt), nullableString(s.LastRunID), nullableString(s.LastError),
		s.ID,
	)
	if err != nil {
		return fmt.Errorf("update schedule: %w", err)
	}
	return nil
}

// Delete removes a Schedule.
func (r *ScheduleRepository) Delete(id string) error {
	_, err := r.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	return nil
}

func scanSchedule(row rowScanner) (*domain.Schedule, error) {
	var (
		s         domain.Schedule
		enabled   int
		lastFire  sql.NullTime
		lastRunID sql.NullString
		lastError sql.NullString
	)
	err := row.Scan(
		&s.ID, &s.AssistantID, &s.Prompt, &s.CronExpr, &s.NLPhrase, &enabled,
		&s.NextFireAt, &lastFire, &lastRunID, &lastError, &s.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("schedule not found")
	}
	if err != nil {
		return nil, fmt.Errorf("scan schedule: %w", err)
	}
	s.Enabled = enabled != 0
	if lastFire.Valid {
		t := lastFire.Time
		s.LastFireAt = &t
	}
	if lastRunID.Valid {
		s.LastRunID = lastRunID.String
	}
	if lastError.Valid {
		s.LastError = lastError.String
	}
	return &s, nil
}

func nullableTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}
