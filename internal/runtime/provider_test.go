package runtime

import (
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// TestProviderProfileCRUD tests provider profile creation, update, delete
func TestProviderProfileCRUD(t *testing.T) {
	_, database, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	repo := db.NewProviderProfileRepository(database)
	settingsRepo := db.NewGlobalSettingsRepository(database)

	// Create profile
	profile := &domain.ProviderProfile{
		ID:        "test-provider",
		Name:      "OpenAI Test",
		Type:      "remote",
		Endpoint:  "https://api.openai.com/v1",
		ModelIDs:  []string{"gpt-4", "gpt-3.5-turbo"},
		SecretRef: "openai-key-ref",
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := repo.Create(profile); err != nil {
		t.Fatalf("Failed to create profile: %v", err)
	}

	// Get by ID
	got, err := repo.GetByID(profile.ID)
	if err != nil {
		t.Fatalf("Failed to get profile: %v", err)
	}
	if got.Name != profile.Name {
		t.Errorf("Expected name %q, got %q", profile.Name, got.Name)
	}
	if len(got.ModelIDs) != 2 {
		t.Errorf("Expected 2 model IDs, got %d", len(got.ModelIDs))
	}

	// List
	profiles, err := repo.List()
	if err != nil {
		t.Fatalf("Failed to list profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Errorf("Expected 1 profile, got %d", len(profiles))
	}

	// Update
	profile.Name = "Updated Name"
	if err := repo.Update(profile); err != nil {
		t.Fatalf("Failed to update profile: %v", err)
	}

	updated, err := repo.GetByID(profile.ID)
	if err != nil {
		t.Fatalf("Failed to get updated profile: %v", err)
	}
	if updated.Name != "Updated Name" {
		t.Errorf("Expected updated name, got %q", updated.Name)
	}

	// Set as default
	if err := settingsRepo.SetLLMDefault(profile.ID, "gpt-4"); err != nil {
		t.Fatalf("Failed to set default: %v", err)
	}

	providerID, modelID := settingsRepo.GetLLMDefault()
	if providerID != profile.ID {
		t.Errorf("Expected provider ID %q, got %q", profile.ID, providerID)
	}
	if modelID != "gpt-4" {
		t.Errorf("Expected model ID gpt-4, got %q", modelID)
	}

	// Delete
	if err := repo.Delete(profile.ID); err != nil {
		t.Fatalf("Failed to delete profile: %v", err)
	}

	_, err = repo.GetByID(profile.ID)
	if err == nil {
		t.Error("Expected error after delete, got nil")
	}
}

// TestAssistantModelPolicy tests that model policy is stored and retrieved with assistants
func TestAssistantModelPolicy(t *testing.T) {
	_, database, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	assistantRepo := db.NewAssistantRepository(database)

	modelPolicy := &domain.ModelPolicy{
		Mode:          "assistant_override",
		Preferred:     "provider1:gpt-4",
		Fallback:      "provider2:gpt-3.5-turbo",
		LocalOnly:     false,
		AllowFallback: true,
	}

	assistant := &domain.AssistantDefinition{
		ID:               "test-model-policy",
		Name:             "Model Policy Test",
		Role:             "test",
		SystemPrompt:     "Test",
		PermissionPolicy: domain.PermissionPolicy{Rules: []domain.PermissionRule{}},
		ModelPolicy:      modelPolicy,
		CreatedAt:        time.Now().UTC(),
	}

	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Retrieve and verify
	got, err := assistantRepo.GetByID(assistant.ID)
	if err != nil {
		t.Fatalf("Failed to get assistant: %v", err)
	}

	if got.ModelPolicy == nil {
		t.Fatal("Expected model policy to be set, got nil")
	}

	if got.ModelPolicy.Mode != "assistant_override" {
		t.Errorf("Expected mode assistant_override, got %q", got.ModelPolicy.Mode)
	}
	if got.ModelPolicy.Preferred != "provider1:gpt-4" {
		t.Errorf("Expected preferred provider1:gpt-4, got %q", got.ModelPolicy.Preferred)
	}
	if got.ModelPolicy.Fallback != "provider2:gpt-3.5-turbo" {
		t.Errorf("Expected fallback provider2:gpt-3.5-turbo, got %q", got.ModelPolicy.Fallback)
	}
}
