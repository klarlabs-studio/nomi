package db

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestRunAttachmentRepository_BatchInsertAndList(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	// Run requires an assistant FK; seed one and a run.
	seedAssistant(t, database, "asst-1")
	runRepo := NewRunRepository(database)
	src := "telegram"
	if err := runRepo.Create(&domain.Run{
		ID: "run-1", Goal: "test", AssistantID: "asst-1", Source: &src,
		Status: domain.RunCreated, PlanVersion: 1,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	repo := NewRunAttachmentRepository(database)
	atts := []*domain.RunAttachment{
		{RunID: "run-1", Kind: "audio", Filename: "voice.ogg", ExternalID: "tg-file-1"},
		{RunID: "run-1", Kind: "image", ExternalID: "tg-file-2", SizeBytes: 1234},
	}
	if err := repo.CreateBatch(atts); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	got, err := repo.ListByRun("run-1")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(got))
	}
	if got[0].Kind != "audio" || got[0].ExternalID != "tg-file-1" {
		t.Fatalf("first attachment shape: %+v", got[0])
	}
	if got[1].Kind != "image" || got[1].SizeBytes != 1234 {
		t.Fatalf("second attachment shape: %+v", got[1])
	}
}

func TestRunAttachmentRepository_RejectsMissingFields(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()
	repo := NewRunAttachmentRepository(database)
	if err := repo.Create(&domain.RunAttachment{Kind: "image"}); err == nil {
		t.Fatal("expected error for missing run_id")
	}
	if err := repo.Create(&domain.RunAttachment{RunID: "run-1"}); err == nil {
		t.Fatal("expected error for missing kind")
	}
}

func TestRunAttachmentRepository_CascadesOnRunDelete(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()
	seedAssistant(t, database, "asst-1")
	runRepo := NewRunRepository(database)
	src := "telegram"
	_ = runRepo.Create(&domain.Run{
		ID: "run-x", Goal: "g", AssistantID: "asst-1", Source: &src,
		Status: domain.RunCreated, PlanVersion: 1,
	})
	repo := NewRunAttachmentRepository(database)
	_ = repo.Create(&domain.RunAttachment{RunID: "run-x", Kind: "image", ExternalID: "f1"})

	if err := runRepo.Delete("run-x"); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	got, err := repo.ListByRun("run-x")
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("attachments should have cascaded on run delete, got %+v", got)
	}
}
