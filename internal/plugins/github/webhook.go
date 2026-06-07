package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"go.klarlabs.de/nomi/internal/plugins"
)

// ReceiveWebhook implements plugins.WebhookReceiver. Parses GitHub webhook
// payloads and fires the corresponding trigger events.
func (p *Plugin) ReceiveWebhook(ctx context.Context, connectionID string, payload []byte, headers map[string]string, onFire plugins.TriggerCallback) error {
	eventType := headers["X-GitHub-Event"]
	if eventType == "" {
		return fmt.Errorf("missing X-GitHub-Event header")
	}

	// Parse the generic payload structure
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}

	switch eventType {
	case "issues":
		return p.handleIssueWebhook(ctx, connectionID, data, onFire)
	case "pull_request":
		return p.handlePRWebhook(ctx, connectionID, data, onFire)
	case "ping":
		// Health-check ping — no action needed
		return nil
	default:
		log.Printf("[github webhook] unhandled event type: %s", eventType)
		return nil
	}
}

func (p *Plugin) handleIssueWebhook(ctx context.Context, connectionID string, data map[string]any, onFire plugins.TriggerCallback) error {
	action, _ := data["action"].(string)
	if action != "opened" {
		return nil // Only fire on new issues
	}

	issue, ok := data["issue"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid issue payload")
	}

	repo, ok := data["repository"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid repository payload")
	}

	num, _ := issue["number"].(float64)
	title, _ := issue["title"].(string)
	body, _ := issue["body"].(string)
	repoName, _ := repo["full_name"].(string)

	ev := plugins.TriggerEvent{
		ConnectionID: connectionID,
		Kind:         TriggerKindIssueOpened,
		Goal:         fmt.Sprintf("New issue in %s: %s", repoName, title),
		Metadata: map[string]interface{}{
			"repo":         repoName,
			"issue_number": int(num),
			"title":        title,
			"body":         body,
		},
	}
	return onFire(ctx, ev)
}

func (p *Plugin) handlePRWebhook(ctx context.Context, connectionID string, data map[string]any, onFire plugins.TriggerCallback) error {
	action, _ := data["action"].(string)
	if action != "review_requested" {
		return nil
	}

	pr, ok := data["pull_request"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid pull_request payload")
	}

	repo, ok := data["repository"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid repository payload")
	}

	num, _ := pr["number"].(float64)
	title, _ := pr["title"].(string)
	body, _ := pr["body"].(string)
	repoName, _ := repo["full_name"].(string)

	ev := plugins.TriggerEvent{
		ConnectionID: connectionID,
		Kind:         TriggerKindPRReviewRequested,
		Goal:         fmt.Sprintf("Review requested on PR %s/%d: %s", repoName, int(num), title),
		Metadata: map[string]interface{}{
			"repo":      repoName,
			"pr_number": int(num),
			"title":     title,
			"body":      body,
		},
	}
	return onFire(ctx, ev)
}
