package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/pkg/statekit"
)

// ErrConcurrentTransition is returned when a run-status update lost a CAS
// race against another writer. Callers should treat it as a benign sign
// that the run already moved on, and skip the duplicate transition (and
// any event they were about to publish for it).
var ErrConcurrentTransition = fmt.Errorf("concurrent run transition detected")

// transitionRun moves a run to a new status. The status update is
// performed under a tx with a CAS-style WHERE status = <from> guard so
// two callers racing to change the same run can't both succeed —
// whichever loses gets ErrConcurrentTransition and skips its work.
//
// State-machine validity is checked before the SQL CAS so an illegal
// transition (e.g. completed → executing) fails fast without touching
// the row.
func (r *Runtime) transitionRun(_ context.Context, run *domain.Run, to domain.RunStatus) error {
	// Load fresh state outside the tx. The DB has MaxOpenConns=1, so
	// any read that goes through r.runRepo.GetByID (which doesn't take
	// a *sql.Tx) would deadlock if invoked while a tx held the conn.
	current, err := r.runRepo.GetByID(run.ID)
	if err != nil {
		slog.Error("transitionRun: failed to load run", "run_id", run.ID, "error", err)
		return fmt.Errorf("failed to load run for transition: %w", err)
	}
	fromStatus := current.Status

	sm := statekit.NewRunStateMachine()
	sm.SetCurrent(current.Status)
	if smErr := sm.Transition(to, nil); smErr != nil {
		return smErr
	}

	// CAS-style write under tx. If a concurrent writer already advanced
	// the row past `current.Status`, the WHERE clause matches zero rows
	// and we return ErrConcurrentTransition so the caller skips its
	// follow-up event publish for the duplicate transition.
	if err := r.db.WithTx(func(tx *sql.Tx) error {
		casErr := r.runRepo.CASUpdateStatusTx(
			tx, run.ID, current.Status, to,
			current.CurrentStepID, current.PlanVersion, current.Source,
		)
		if casErr == sql.ErrNoRows {
			return ErrConcurrentTransition
		}
		return casErr
	}); err != nil {
		return err
	}

	now := time.Now().UTC()
	slog.Info("transitionRun", "run_id", run.ID, "from", fromStatus, "to", to)

	run.Status = to
	run.UpdatedAt = now
	run.CurrentStepID = current.CurrentStepID
	run.PlanVersion = current.PlanVersion
	run.Source = current.Source
	return nil
}

// transitionStep transitions a step to a new state
func (r *Runtime) transitionStep(_ context.Context, step *domain.Step, to domain.StepStatus) error {
	sm := statekit.NewStepStateMachine()
	sm.SetCurrent(step.Status)
	if err := sm.Transition(to, nil); err != nil {
		slog.Warn("step transition failed", "step_id", step.ID, "from", step.Status, "to", to, "error", err)
		return err
	}

	step.Status = to
	step.UpdatedAt = time.Now().UTC()
	slog.Info("step transitioned", "step_id", step.ID, "run_id", step.RunID, "from", step.Status, "to", to)
	return r.stepRepo.Update(step)
}

// transitionStepAtomic updates a step's status AND writes an event row in a
// single transaction, broadcasting the event to SSE subscribers only after
// the commit succeeds. Without this, a crash between the row update and the
// event insert would produce a silent state transition: the step moves on
// in the DB but no observer ever saw the completed/failed event, and the
// UI would permanently show stale data.
func (r *Runtime) transitionStepAtomic(
	_ context.Context,
	step *domain.Step,
	to domain.StepStatus,
	eventType domain.EventType,
	payload map[string]interface{},
) error {
	// Validate the transition against the state machine before touching the DB.
	sm := statekit.NewStepStateMachine()
	sm.SetCurrent(step.Status)
	if err := sm.Transition(to, nil); err != nil {
		return err
	}

	now := time.Now().UTC()
	event := &domain.Event{
		ID:        uuid.New().String(),
		Type:      eventType,
		RunID:     step.RunID,
		StepID:    &step.ID,
		Payload:   payload,
		Timestamp: now,
	}
	stepCopy := *step
	stepCopy.Status = to
	stepCopy.UpdatedAt = now

	if err := r.db.WithTx(func(tx *sql.Tx) error {
		if err := r.stepRepo.UpdateTx(tx, &stepCopy); err != nil {
			return err
		}
		return db.NewEventRepository(r.db).CreateTx(tx, event)
	}); err != nil {
		return fmt.Errorf("atomic step transition failed: %w", err)
	}

	// Commit succeeded; mutate caller's struct and notify subscribers.
	step.Status = to
	step.UpdatedAt = now
	r.eventBus.Broadcast(event)
	return nil
}

// failRun marks a run as failed and updates the current step. Transitions go
// through the state machine so invariants (e.g. "failed only from an active
// state") are enforced; if the current state has no valid edge to RunFailed,
// we write the raw status as a last-resort fallback and log the anomaly.
func (r *Runtime) failRun(ctx context.Context, run *domain.Run, runErr error) {
	slog.Error("run failed", "run_id", run.ID, "status", run.Status, "error", runErr)
	if err := r.transitionRun(ctx, run, domain.RunFailed); err != nil {
		slog.Error("failRun: state machine rejected transition", "run_id", run.ID, "error", err)
		// State machine rejected the transition. This indicates a logic
		// error (e.g. failing an already-terminal run); persist the status
		// anyway so the user sees the failure.
		run.Status = domain.RunFailed
		run.UpdatedAt = time.Now().UTC()
		_ = r.runRepo.Update(run)
	}

	if run.CurrentStepID != nil {
		if step, err := r.stepRepo.GetByID(*run.CurrentStepID); err == nil {
			if step.Error == nil {
				step.Error = strPtr(runErr.Error())
			}
			if tErr := r.transitionStep(ctx, step, domain.StepFailed); tErr != nil {
				// Step was not in a state that permits -> failed; force-update.
				step.Status = domain.StepFailed
				step.UpdatedAt = time.Now().UTC()
				_ = r.stepRepo.Update(step)
			}
		}
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventRunFailed, run.ID, nil, map[string]interface{}{
		"error": runErr.Error(),
	})
	r.limiter.ForgetRun(run.ID)
}
