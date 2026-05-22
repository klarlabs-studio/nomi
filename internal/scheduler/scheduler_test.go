package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

type fakeTrigger struct {
	mu    sync.Mutex
	fires []string // assistant IDs in order
	err   error
}

func (f *fakeTrigger) CreateRunFromSource(_ context.Context, _, assistantID, _ string) (*domain.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.fires = append(f.fires, assistantID)
	return &domain.Run{ID: "run-" + assistantID}, nil
}

func (f *fakeTrigger) firesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.fires))
	copy(out, f.fires)
	return out
}

func newTestRepo(t *testing.T) *db.ScheduleRepository {
	t.Helper()
	conn, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// schedules has an FK to assistants — seed a minimum-viable row.
	if _, err := conn.Exec(`INSERT INTO assistants (id, name, role, system_prompt, channels, channel_configs, capabilities, contexts, memory_policy, permission_policy, model_policy, executor_backend, sandbox_image, created_at) VALUES ('a1', 'a1', 'tester', 'sp', '[]', '[]', '[]', '[]', '{}', '{}', 'null', 'local', '', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
	return db.NewScheduleRepository(conn)
}

func TestValidateCron(t *testing.T) {
	s := New(nil, nil)
	if err := s.ValidateCron("0 9 * * *"); err != nil {
		t.Fatalf("valid expression rejected: %v", err)
	}
	if err := s.ValidateCron("not-a-cron"); err == nil {
		t.Fatal("expected error for malformed cron")
	}
}

func TestNewScheduleSeedsNextFire(t *testing.T) {
	s := New(nil, nil)
	sch, err := s.NewSchedule("a1", "hello", "*/5 * * * *")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sch.ID == "" || sch.AssistantID != "a1" || sch.Prompt != "hello" || !sch.Enabled {
		t.Fatalf("unexpected schedule: %+v", sch)
	}
	if sch.NextFireAt.IsZero() {
		t.Fatal("NextFireAt was not populated")
	}
}

func TestNewScheduleRejectsBlanks(t *testing.T) {
	s := New(nil, nil)
	if _, err := s.NewSchedule("", "p", "* * * * *"); err == nil {
		t.Fatal("expected error for empty assistant")
	}
	if _, err := s.NewSchedule("a1", "", "* * * * *"); err == nil {
		t.Fatal("expected error for empty prompt")
	}
	if _, err := s.NewSchedule("a1", "p", "not-a-cron"); err == nil {
		t.Fatal("expected error for bad cron")
	}
}

func TestTickFiresDueSchedules(t *testing.T) {
	repo := newTestRepo(t)
	trigger := &fakeTrigger{}
	s := New(repo, trigger)

	// Force NextFireAt into the past so DueBefore picks it up.
	sch, err := s.NewSchedule("a1", "hi", "*/1 * * * *")
	if err != nil {
		t.Fatalf("new schedule: %v", err)
	}
	sch.NextFireAt = time.Now().UTC().Add(-time.Minute)
	if err := repo.Create(sch); err != nil {
		t.Fatalf("create: %v", err)
	}

	s.tick(context.Background())

	fires := trigger.firesSnapshot()
	if len(fires) != 1 || fires[0] != "a1" {
		t.Fatalf("expected one fire on a1, got %v", fires)
	}

	reloaded, err := repo.GetByID(sch.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.LastFireAt == nil {
		t.Fatal("LastFireAt not set after fire")
	}
	if !reloaded.NextFireAt.After(time.Now()) {
		t.Fatalf("NextFireAt didn't advance: %v", reloaded.NextFireAt)
	}
	if reloaded.LastRunID != "run-a1" {
		t.Fatalf("LastRunID not recorded: %q", reloaded.LastRunID)
	}
}

func TestTickRecordsTriggerError(t *testing.T) {
	repo := newTestRepo(t)
	trigger := &fakeTrigger{err: errors.New("boom")}
	s := New(repo, trigger)

	sch, _ := s.NewSchedule("a1", "hi", "*/1 * * * *")
	sch.NextFireAt = time.Now().UTC().Add(-time.Minute)
	_ = repo.Create(sch)

	s.tick(context.Background())

	reloaded, _ := repo.GetByID(sch.ID)
	if reloaded.LastError == "" || reloaded.LastError != "boom" {
		t.Fatalf("expected LastError 'boom', got %q", reloaded.LastError)
	}
	// Should still advance NextFireAt so we don't busy-loop on the same row.
	if !reloaded.NextFireAt.After(time.Now()) {
		t.Fatalf("NextFireAt didn't advance on error: %v", reloaded.NextFireAt)
	}
}

func TestTickSkipsDisabled(t *testing.T) {
	repo := newTestRepo(t)
	trigger := &fakeTrigger{}
	s := New(repo, trigger)

	sch, _ := s.NewSchedule("a1", "hi", "*/1 * * * *")
	sch.NextFireAt = time.Now().UTC().Add(-time.Minute)
	sch.Enabled = false
	_ = repo.Create(sch)

	s.tick(context.Background())

	if len(trigger.firesSnapshot()) != 0 {
		t.Fatal("disabled schedule should not fire")
	}
}
