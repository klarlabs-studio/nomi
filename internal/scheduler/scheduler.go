// Package scheduler runs schedules: a background ticker queries the
// schedule store for entries whose next_fire_at has elapsed and triggers
// a fresh Run through the runtime, then computes the next fire time
// from the cron expression.
//
// Missed-fire policy: skip. If the daemon was down for a window that
// covered N fires, the schedule fires once on the next tick (advancing
// to the next future cron slot), not N times. Catch-up runs are almost
// never what an automation owner wants — they'd see a thundering herd
// of stale Runs on daemon restart.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// RunTrigger creates a new Run on behalf of a schedule. Matches the
// runtime's CreateRunFromSource shape so we don't introduce a new code
// path through Runtime — schedules look like a regular connector source
// in the audit trail.
type RunTrigger interface {
	CreateRunFromSource(ctx context.Context, goal, assistantID, source string) (*domain.Run, error)
}

// Scheduler polls the schedule store on a fixed cadence and fires due
// schedules through the supplied RunTrigger. Safe to call Start once;
// subsequent calls are no-ops. Stop cancels the background ticker.
type Scheduler struct {
	repo    *db.ScheduleRepository
	trigger RunTrigger
	parser  cron.Parser

	tickInterval time.Duration

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// SourceName labels schedule-triggered Runs in events + audit. Matches
// the "source" string a Telegram or Slack message would carry.
const SourceName = "schedule"

// DefaultTickInterval is the scheduler's polling cadence. 30s balances
// "next-fire-at responsiveness within a minute" against not hammering
// SQLite. Schedules with sub-minute cron precision still work — they
// just may fire up to one tick late.
const DefaultTickInterval = 30 * time.Second

// New constructs a Scheduler. The trigger is invoked when a schedule
// fires; in production this is the Nomi runtime. Tests can pass a fake
// RunTrigger to assert fire behavior without spinning up a real run.
func New(repo *db.ScheduleRepository, trigger RunTrigger) *Scheduler {
	return &Scheduler{
		repo:         repo,
		trigger:      trigger,
		parser:       cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		tickInterval: DefaultTickInterval,
	}
}

// WithTickInterval overrides the default polling cadence. Useful for
// tests that need to observe a fire within milliseconds.
func (s *Scheduler) WithTickInterval(d time.Duration) *Scheduler {
	s.tickInterval = d
	return s
}

// Start launches the background ticker. The supplied context is the
// scheduler's lifetime — cancelling it stops the ticker. Returns
// immediately. Idempotent: a second Start is a no-op while the first is
// still running.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	tickCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()

	go s.loop(tickCtx)
}

// Stop cancels the ticker. Safe to call when not running.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
}

// ValidateCron returns an error if expr cannot be parsed. Callers use
// this to refuse schedule create/update before persisting an expression
// the ticker would skip every iteration.
func (s *Scheduler) ValidateCron(expr string) error {
	_, err := s.parser.Parse(expr)
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return nil
}

// NextFire computes the next fire time after `from` for the given cron
// expression. Used at create/update to populate next_fire_at and after
// each fire to advance the schedule. Returns the zero time when expr is
// invalid; callers should ValidateCron first.
func (s *Scheduler) NextFire(expr string, from time.Time) time.Time {
	sched, err := s.parser.Parse(expr)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(from)
}

func (s *Scheduler) loop(ctx context.Context) {
	// Tick once immediately so a freshly-started scheduler picks up any
	// schedules that became due while the daemon was down. Subsequent
	// ticks fire on the interval.
	s.tick(ctx)
	t := time.NewTicker(s.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick fires every due schedule once. Errors from a single schedule
// (cron parse failure, run trigger failure, store update failure) are
// logged + recorded on the schedule's last_error field but don't abort
// the tick — other schedules in the same window still run.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	due, err := s.repo.DueBefore(now)
	if err != nil {
		slog.Error("scheduler: list due schedules failed", "error", err)
		return
	}
	for _, sch := range due {
		s.fire(ctx, sch, now)
	}
}

func (s *Scheduler) fire(ctx context.Context, sch *domain.Schedule, now time.Time) {
	next := s.NextFire(sch.CronExpr, now)
	if next.IsZero() {
		// Bad cron expression — recompute every tick won't help. Disable
		// to stop the noise and surface the cause via last_error.
		sch.Enabled = false
		sch.LastError = "invalid cron expression; schedule disabled"
		sch.NextFireAt = now.Add(24 * time.Hour) // park far future
		if err := s.repo.Update(sch); err != nil {
			slog.Error("scheduler: failed to disable invalid schedule", "schedule_id", sch.ID, "error", err)
		}
		return
	}

	run, err := s.trigger.CreateRunFromSource(ctx, sch.Prompt, sch.AssistantID, SourceName)
	fired := now
	sch.LastFireAt = &fired
	sch.NextFireAt = next
	if err != nil {
		sch.LastError = err.Error()
		slog.Error("scheduler: fire failed", "schedule_id", sch.ID, "error", err)
	} else {
		sch.LastError = ""
		if run != nil {
			sch.LastRunID = run.ID
		}
	}
	if updateErr := s.repo.Update(sch); updateErr != nil {
		slog.Error("scheduler: persist fire result failed", "schedule_id", sch.ID, "error", updateErr)
	}
}

// NewSchedule constructs a Schedule with NextFireAt seeded from the cron
// expression. Returns an error if the expression doesn't parse. Used by
// the REST handler and tests.
func (s *Scheduler) NewSchedule(assistantID, prompt, cronExpr string) (*domain.Schedule, error) {
	if assistantID == "" {
		return nil, errors.New("assistant_id is required")
	}
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if err := s.ValidateCron(cronExpr); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &domain.Schedule{
		ID:          uuid.New().String(),
		AssistantID: assistantID,
		Prompt:      prompt,
		CronExpr:    cronExpr,
		Enabled:     true,
		NextFireAt:  s.NextFire(cronExpr, now),
		CreatedAt:   now,
	}, nil
}
