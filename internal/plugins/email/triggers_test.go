package email

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins/email/transport"
)

func TestTriggerRule_FiltersAreCaseInsensitive(t *testing.T) {
	rule := domain.TriggerRule{
		AssistantID:     "asst-1",
		FromContains:    "BOSS@",
		SubjectContains: "urgent",
		Enabled:         true,
	}
	m := transport.Message{
		From:    "Big Boss <boss@example.com>",
		Subject: "URGENT: prod is down",
		Body:    "fix it",
	}
	if !matchesTriggerRule(&rule, m) {
		t.Fatal("case-insensitive filters should match")
	}
	// A non-matching From should fail the rule even if subject matches.
	m.From = "bob@example.com"
	if matchesTriggerRule(&rule, m) {
		t.Fatal("rule should not match when From filter fails")
	}
}

func TestTriggerRule_DisabledNeverMatches(t *testing.T) {
	rule := domain.TriggerRule{AssistantID: "asst", Enabled: false}
	m := transport.Message{From: "a@b.c"}
	if matchesTriggerRule(&rule, m) {
		t.Fatal("disabled rule should never match")
	}
}

func TestTriggerRule_EmptyAssistantIDRejected(t *testing.T) {
	// A rule with no target assistant_id can't route anywhere — treat
	// as "never matches" so the channel-role flow still gets the message.
	rule := domain.TriggerRule{AssistantID: "", Enabled: true, FromContains: "boss@"}
	m := transport.Message{From: "boss@example.com"}
	if matchesTriggerRule(&rule, m) {
		t.Fatal("rule without assistant_id should not match")
	}
}

func TestTriggerRule_CatchAll(t *testing.T) {
	// A rule with all filters empty + enabled + an assistant acts as a
	// catch-all — useful for "fire every email at the triage assistant".
	rule := domain.TriggerRule{AssistantID: "asst-triage", Enabled: true}
	m := transport.Message{From: "anyone@example.com", Subject: "Hi"}
	if !matchesTriggerRule(&rule, m) {
		t.Fatal("catch-all rule should match everything")
	}
}

func TestFirstMatchingRule_FirstWins(t *testing.T) {
	rules := []domain.TriggerRule{
		{Name: "specific", AssistantID: "asst-1", FromContains: "alert", Enabled: true},
		{Name: "catch-all", AssistantID: "asst-2", Enabled: true},
	}
	got := firstMatchingRule(rules, transport.Message{From: "alerts@example.com"})
	if got == nil || got.Name != "specific" {
		t.Fatalf("expected 'specific' rule first, got %+v", got)
	}
	got = firstMatchingRule(rules, transport.Message{From: "random@example.com"})
	if got == nil || got.Name != "catch-all" {
		t.Fatalf("expected fall-through to catch-all, got %+v", got)
	}
}

func TestTriggerRulesFor_ParsesValidShape(t *testing.T) {
	cfg := map[string]interface{}{
		"trigger_rules": []interface{}{
			map[string]interface{}{
				"name":             "alerts",
				"assistant_id":     "asst-1",
				"subject_contains": "ALERT",
				"enabled":          true,
			},
		},
	}
	rules := triggerRulesFor(cfg)
	if len(rules) != 1 || rules[0].Name != "alerts" || !rules[0].Enabled {
		t.Fatalf("parse failed: %+v", rules)
	}
}

func TestTriggerRulesFor_SkipsMalformed(t *testing.T) {
	cfg := map[string]interface{}{
		"trigger_rules": []interface{}{
			"not-a-map",
			map[string]interface{}{"name": "ok", "assistant_id": "asst", "enabled": true},
		},
	}
	rules := triggerRulesFor(cfg)
	if len(rules) != 1 {
		t.Fatalf("expected 1 valid rule parsed, got %d", len(rules))
	}
}

func TestResolveThreadKey_PrefersReferencesRoot(t *testing.T) {
	m := transport.Message{
		MessageID:  "<self>",
		InReplyTo:  "<parent>",
		References: []string{"<root>", "<parent>"},
	}
	if got := resolveThreadKey(m); got != "<root>" {
		t.Fatalf("expected <root>, got %s", got)
	}
	// Falls back to InReplyTo when no References chain.
	m.References = nil
	if got := resolveThreadKey(m); got != "<parent>" {
		t.Fatalf("expected <parent>, got %s", got)
	}
	// Falls back to MessageID for brand-new threads.
	m.InReplyTo = ""
	if got := resolveThreadKey(m); got != "<self>" {
		t.Fatalf("expected <self>, got %s", got)
	}
}
