package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

func newTestDBLite(t *testing.T) (*db.DB, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "nomi-enrich-*.db")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	_ = f.Close()
	database, err := db.New(db.Config{Path: f.Name()})
	if err != nil {
		_ = os.Remove(f.Name())
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		_ = database.Close()
		_ = os.Remove(f.Name())
		t.Fatalf("migrate: %v", err)
	}
	return database, func() {
		_ = database.Close()
		_ = os.Remove(f.Name())
	}
}

func seedAssistantAndRun(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO assistants (id, name, role, system_prompt, capabilities, channels, contexts, memory_policy, permission_policy)
		 VALUES ('asst-1', 'Test', 'assistant', '', '[]', '[]', '[]', '{}', '{}')`,
	); err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
	src := "telegram"
	runRepo := db.NewRunRepository(database)
	if err := runRepo.Create(&domain.Run{
		ID: runID, Goal: "original goal", AssistantID: "asst-1", Source: &src,
		Status: domain.RunCreated, PlanVersion: 1,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func TestEnrich_NoAttachments_ReturnsOriginalGoal(t *testing.T) {
	database, cleanup := newTestDBLite(t)
	defer cleanup()
	seedAssistantAndRun(t, database, "run-1")

	svc := NewEnrichmentService(db.NewRunAttachmentRepository(database), tools.NewExecutor(tools.NewRegistry()), nil)
	got := svc.Enrich(context.Background(), "run-1", "original")
	if got != "original" {
		t.Fatalf("expected pass-through, got %q", got)
	}
}

func TestEnrich_AnnouncesNonAudioAttachmentsWithoutBackend(t *testing.T) {
	database, cleanup := newTestDBLite(t)
	defer cleanup()
	seedAssistantAndRun(t, database, "run-2")

	repo := db.NewRunAttachmentRepository(database)
	_ = repo.Create(&domain.RunAttachment{RunID: "run-2", Kind: "image", Filename: "cat.png", ContentType: "image/png"})
	_ = repo.Create(&domain.RunAttachment{RunID: "run-2", Kind: "document", Filename: "report.pdf", ContentType: "application/pdf"})

	svc := NewEnrichmentService(repo, tools.NewExecutor(tools.NewRegistry()), nil)
	got := svc.Enrich(context.Background(), "run-2", "describe these")
	// Both attachments should produce announcement lines so the planner
	// at least knows they exist, even before vision/document backends ship.
	if !contains(got, "image attachment") || !contains(got, "document attachment") {
		t.Fatalf("expected attachment announcements, got %q", got)
	}
}

func TestEnrich_TranscribesAudioWhenBackendAvailable(t *testing.T) {
	database, cleanup := newTestDBLite(t)
	defer cleanup()
	seedAssistantAndRun(t, database, "run-3")

	// Stand up a tiny HTTP server hosting fake audio bytes.
	audioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0x01, 0x02, 0x03})
	}))
	defer audioSrv.Close()

	repo := db.NewRunAttachmentRepository(database)
	_ = repo.Create(&domain.RunAttachment{RunID: "run-3", Kind: "audio", URL: audioSrv.URL, Filename: "voice.ogg"})

	// Register a fake media.transcribe tool.
	reg := tools.NewRegistry()
	if err := reg.Register(&fakeTranscribeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	svc := NewEnrichmentService(repo, tools.NewExecutor(reg), nil)
	got := svc.Enrich(context.Background(), "run-3", "what did they say?")
	if !contains(got, "voice transcript") || !contains(got, "TRANSCRIPT") {
		t.Fatalf("expected transcript folded into goal, got %q", got)
	}
}

func TestEnrich_AudioWithoutURLIsSkipped(t *testing.T) {
	database, cleanup := newTestDBLite(t)
	defer cleanup()
	seedAssistantAndRun(t, database, "run-4")

	repo := db.NewRunAttachmentRepository(database)
	_ = repo.Create(&domain.RunAttachment{RunID: "run-4", Kind: "audio", ExternalID: "tg-file-id"})

	reg := tools.NewRegistry()
	_ = reg.Register(&fakeTranscribeTool{})
	svc := NewEnrichmentService(repo, tools.NewExecutor(reg), nil)
	got := svc.Enrich(context.Background(), "run-4", "original")
	// Without a URL we can't fetch — expect the original goal back so
	// the run still proceeds (Telegram getFile resolution is a follow-up).
	if got != "original" {
		t.Fatalf("audio without URL should be skipped, got %q", got)
	}
}

// fakeTranscribeTool stands in for media.transcribe so the test
// doesn't need the real Whisper binary.
type fakeTranscribeTool struct{}

func (f *fakeTranscribeTool) Name() string       { return "media.transcribe" }
func (f *fakeTranscribeTool) Capability() string { return "media.stt" }
func (f *fakeTranscribeTool) Execute(_ context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{
		"transcript":        "TRANSCRIPT",
		"detected_language": "en",
	}, nil
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && stringContains(s, sub)))
}
func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
