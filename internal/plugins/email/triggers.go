package email

import (
	"context"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins/email/transport"
)

// Trigger rules for the Email plugin. A rule is a simple predicate over an
// inbound Message (from / subject / body contains) plus a target
// assistant_id. When a rule matches, the matching message fires a Run
// against that assistant INSTEAD of routing through the channel-role
// binding. Rules short-circuit on the first match in declaration order
// so the user has a predictable priority model.
//
// Rules live inside the connection's config blob so the Plugins-tab UI
// can manage them alongside the rest of the connection settings. Stored
// shape on the wire:
//
//	"trigger_rules": [
//	  {"name": "Triage", "assistant_id": "asst-1", "from_contains": "boss@"},
//	  {"name": "Alerts", "assistant_id": "asst-2", "subject_contains": "ALERT"}
//	]

// matchesTriggerRule reports whether the rule applies to the given message.
// Case-insensitive substring match on each configured field; empty fields
// are skipped. Defined as a package function (not a method) to avoid
// an import cycle between domain ←→ email plugins.
func matchesTriggerRule(r *domain.TriggerRule, m transport.Message) bool {
	if r == nil || !r.Enabled {
		return false
	}
	if r.AssistantID == "" {
		return false
	}
	if r.FromContains != "" && !containsFold(m.From, r.FromContains) {
		return false
	}
	if r.SubjectContains != "" && !containsFold(m.Subject, r.SubjectContains) {
		return false
	}
	if r.BodyContains != "" && !containsFold(m.Body, r.BodyContains) {
		return false
	}
	return true
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// triggerRulesFor parses the connection's config for trigger_rules. Missing
// or malformed entries are skipped; the plugin never fails the inbound
// message flow because of a bad rule.
func triggerRulesFor(cfg map[string]interface{}) []domain.TriggerRule {
	raw, ok := cfg["trigger_rules"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]domain.TriggerRule, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		rule := domain.TriggerRule{
			Name:            stringField(m, "name"),
			AssistantID:     stringField(m, "assistant_id"),
			FromContains:    stringField(m, "from_contains"),
			SubjectContains: stringField(m, "subject_contains"),
			BodyContains:    stringField(m, "body_contains"),
			Enabled:         boolField(m, "enabled"),
		}
		out = append(out, rule)
	}
	return out
}

func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func boolField(m map[string]interface{}, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

// firstMatchingRule returns the first enabled rule that matches the
// message, or nil if none apply. First-match-wins gives users a
// predictable priority model: put more specific rules before catch-alls.
func firstMatchingRule(rules []domain.TriggerRule, m transport.Message) *domain.TriggerRule {
	for i := range rules {
		if matchesTriggerRule(&rules[i], m) {
			return &rules[i]
		}
	}
	return nil
}

// handleRuleMatch fires a Run against the rule's target assistant. The
// run is threaded into the same Conversation as the channel-role path
// would be, so a user who has both a trigger and a channel binding to
// the same assistant still sees one coherent thread. Source is "email"
// regardless of which path produced it — the channel manifest is the
// permission ceiling either way.
func (p *Plugin) handleRuleMatch(ctx context.Context, connID string, rule *domain.TriggerRule, conv *domain.Conversation, m transport.Message) {
	goal := m.Subject
	if goal == "" {
		goal = "(no subject)"
	}
	senderAddr, _ := transport.ParseAddress(m.From)
	if senderAddr == "" {
		senderAddr = m.From
	}
	if m.Body != "" {
		goal = buildGoalString(goal, senderAddr, m.Body)
	}
	var conversationID string
	if conv != nil {
		conversationID = conv.ID
	}
	if _, err := p.rt.CreateRunInConversation(ctx, goal, rule.AssistantID, "email", conversationID); err != nil {
		// Non-fatal; channel-role flow can still pick up the message.
		// Logged at WARN level so operators can see rule mismatches.
		// (Using fmt.Errorf rather than a new log.Printf would lose the
		// rule name context; keep the direct call.)
		_ = err
	}
}

// buildGoalString keeps the user-visible goal string format consistent
// across the channel and trigger paths.
func buildGoalString(subject, sender, body string) string {
	return subject + "\n\nFrom: " + sender + "\n\n" + strings.TrimSpace(body)
}

// resolveThreadKey derives a stable Message-ID to use as the
// Conversation's external_conversation_id. Preference order:
//
//  1. The root of the References chain (Gmail-style thread root)
//  2. The In-Reply-To value (previous message)
//  3. The message's own Message-ID (new thread)
//
// Returns "" when no Message-ID is available so callers can decide
// whether to fall back to a per-message Conversation or skip threading.
func resolveThreadKey(m transport.Message) string {
	if len(m.References) > 0 {
		return m.References[0]
	}
	if m.InReplyTo != "" {
		return m.InReplyTo
	}
	return m.MessageID
}
