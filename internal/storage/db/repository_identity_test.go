package db

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestChannelIdentityRepository_CreateFindAndEnforce(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	identRepo := NewChannelIdentityRepository(database)

	_ = connRepo.Create(&domain.Connection{
		ID: "conn-1", PluginID: "com.nomi.telegram", Name: "bot", Enabled: true,
	})

	// Empty allowlist → IsAllowed = false.
	ok, err := identRepo.IsAllowed("com.nomi.telegram", "conn-1", "user-1", "")
	if err != nil {
		t.Fatalf("IsAllowed: %v", err)
	}
	if ok {
		t.Fatal("empty allowlist should block unknown senders")
	}

	// Add an entry; IsAllowed = true.
	if err := identRepo.Create(&domain.ChannelIdentity{
		PluginID:           "com.nomi.telegram",
		ConnectionID:       "conn-1",
		ExternalIdentifier: "user-1",
		DisplayName:        "Alice",
		Enabled:            true,
	}); err != nil {
		t.Fatalf("Create identity: %v", err)
	}
	ok, _ = identRepo.IsAllowed("com.nomi.telegram", "conn-1", "user-1", "")
	if !ok {
		t.Fatal("allowlisted sender should be allowed")
	}

	// Disabled entries do not allow.
	got, _ := identRepo.FindByExternal("com.nomi.telegram", "conn-1", "user-1")
	got.Enabled = false
	_ = identRepo.Update(got)
	ok, _ = identRepo.IsAllowed("com.nomi.telegram", "conn-1", "user-1", "")
	if ok {
		t.Fatal("disabled identity should not be allowed")
	}
}

func TestChannelIdentityRepository_PerAssistantFilter(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	identRepo := NewChannelIdentityRepository(database)
	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.telegram", Name: "b", Enabled: true})

	// AllowedAssistants limits which assistants this sender can reach.
	_ = identRepo.Create(&domain.ChannelIdentity{
		PluginID:           "com.nomi.telegram",
		ConnectionID:       "c",
		ExternalIdentifier: "user-1",
		AllowedAssistants:  []string{"asst-a"},
		Enabled:            true,
	})

	ok, _ := identRepo.IsAllowed("com.nomi.telegram", "c", "user-1", "asst-a")
	if !ok {
		t.Fatal("listed assistant should be allowed")
	}
	ok, _ = identRepo.IsAllowed("com.nomi.telegram", "c", "user-1", "asst-b")
	if ok {
		t.Fatal("non-listed assistant should be blocked")
	}

	// Empty AllowedAssistants list means "any assistant".
	ident, _ := identRepo.FindByExternal("com.nomi.telegram", "c", "user-1")
	ident.AllowedAssistants = nil
	_ = identRepo.Update(ident)
	ok, _ = identRepo.IsAllowed("com.nomi.telegram", "c", "user-1", "asst-anywhere")
	if !ok {
		t.Fatal("nil allowed_assistants should allow any assistant")
	}
}

func TestChannelIdentityRepository_CascadesOnConnectionDelete(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	identRepo := NewChannelIdentityRepository(database)

	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.telegram", Name: "b", Enabled: true})
	_ = identRepo.Create(&domain.ChannelIdentity{
		PluginID: "com.nomi.telegram", ConnectionID: "c", ExternalIdentifier: "u", Enabled: true,
	})

	if err := connRepo.Delete("c"); err != nil {
		t.Fatalf("Delete connection: %v", err)
	}
	got, _ := identRepo.ListByConnection("c")
	if len(got) != 0 {
		t.Fatalf("identities should have cascaded on connection delete, got %+v", got)
	}
}

func TestFirstContactPolicyValidation(t *testing.T) {
	for _, p := range []domain.FirstContactPolicy{
		domain.FirstContactDrop,
		domain.FirstContactReplyRequestAccess,
		domain.FirstContactQueueApproval,
	} {
		if !p.IsValid() {
			t.Fatalf("%s should be valid", p)
		}
	}
	if (domain.FirstContactPolicy("weird")).IsValid() {
		t.Fatal("unknown policy should be invalid")
	}
}
