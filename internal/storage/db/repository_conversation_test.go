package db

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestConversationRepository_FindOrCreate_IdempotentByNaturalKey(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	// FK constraints require an assistant and a connection to exist.
	seedAssistant(t, database, "asst-1")
	connRepo := NewConnectionRepository(database)
	_ = connRepo.Create(&domain.Connection{
		ID: "conn-1", PluginID: "com.nomi.telegram", Name: "bot", Enabled: true,
	})

	convRepo := NewConversationRepository(database)

	// First call creates.
	c1, created1, err := convRepo.FindOrCreate("com.nomi.telegram", "conn-1", "chat-42", "asst-1", nil)
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	if !created1 {
		t.Fatal("first call should have created a new conversation")
	}
	if c1.AssistantID != "asst-1" {
		t.Fatalf("assistant: %s", c1.AssistantID)
	}

	// Second call with same natural key returns the same row, created=false.
	c2, created2, err := convRepo.FindOrCreate("com.nomi.telegram", "conn-1", "chat-42", "asst-1", nil)
	if err != nil {
		t.Fatalf("FindOrCreate (2nd): %v", err)
	}
	if created2 {
		t.Fatal("second call should be a lookup, not a create")
	}
	if c2.ID != c1.ID {
		t.Fatalf("expected same id, got %s vs %s", c1.ID, c2.ID)
	}
}

func TestConversationRepository_ListByAssistant_OrderedByUpdated(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedAssistant(t, database, "asst-1")
	connRepo := NewConnectionRepository(database)
	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.telegram", Name: "b", Enabled: true})

	convRepo := NewConversationRepository(database)
	a, _, _ := convRepo.FindOrCreate("com.nomi.telegram", "c", "chat-a", "asst-1", nil)
	b, _, _ := convRepo.FindOrCreate("com.nomi.telegram", "c", "chat-b", "asst-1", nil)

	// Touch b so it sorts first.
	if err := convRepo.Touch(b.ID, nil); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	got, err := convRepo.ListByAssistant("asst-1", 10)
	if err != nil {
		t.Fatalf("ListByAssistant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(got))
	}
	if got[0].ID != b.ID {
		t.Fatalf("expected touched conversation first, got %s then %s", got[0].ID, a.ID)
	}
}

func TestRunRepository_RoundTripsConversationID(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedAssistant(t, database, "asst-1")
	connRepo := NewConnectionRepository(database)
	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.telegram", Name: "b", Enabled: true})

	convRepo := NewConversationRepository(database)
	conv, _, _ := convRepo.FindOrCreate("com.nomi.telegram", "c", "chat-1", "asst-1", nil)

	runRepo := NewRunRepository(database)
	convID := conv.ID
	src := "telegram"
	run := &domain.Run{
		ID:             "run-1",
		Goal:           "hello",
		AssistantID:    "asst-1",
		Source:         &src,
		ConversationID: &convID,
		Status:         domain.RunCreated,
		PlanVersion:    1,
	}
	if err := runRepo.Create(run); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	got, err := runRepo.GetByID("run-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ConversationID == nil || *got.ConversationID != convID {
		t.Fatalf("conversation_id lost on round-trip: %+v", got.ConversationID)
	}
}
