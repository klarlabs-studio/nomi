// Package webhooks implements the Tunneled Inbound Receiver: a public
// HTTP surface that accepts verified webhook payloads from external
// services and routes them into plugin triggers.
//
// Endpoints are unauthenticated by design — signature verification
// (HMAC, Ed25519, or provider-specific schemes) is the security boundary.
package webhooks

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// RouterConfig bundles the dependencies the webhook router needs.
type RouterConfig struct {
	PluginRegistry *plugins.Registry
	ConnectionRepo *db.ConnectionRepository
	BindingRepo    *db.AssistantBindingRepository
	Secrets        secrets.Store
	Runtime        interface {
		CreateRunFromSource(ctx context.Context, goal, assistantID, source string) (*domain.Run, error)
	}
	EventBus EventPublisher
}

// EventPublisher is the subset of events.EventBus that the webhook router needs.
type EventPublisher interface {
	Publish(ctx context.Context, eventType domain.EventType, runID string, stepID *string, payload map[string]interface{}) (*domain.Event, error)
}

// Router is the webhook HTTP surface.
type Router struct {
	cfg RouterConfig
}

// NewRouter creates a webhook router.
func NewRouter(cfg RouterConfig) *Router {
	return &Router{cfg: cfg}
}

// Mount registers public webhook routes on the supplied gin engine.
// These routes skip the auth middleware — external services have no
// bearer token.
func (r *Router) Mount(engine *gin.Engine) {
	engine.POST("/webhooks/:plugin_id/:connection_id", r.handleWebhook)
	engine.GET("/webhooks/status", r.handleStatus)
}

// handleWebhook receives a webhook payload, verifies its signature,
// checks the event allowlist, and dispatches to the target plugin.
func (r *Router) handleWebhook(c *gin.Context) {
	pluginID := c.Param("plugin_id")
	connectionID := c.Param("connection_id")

	// 1. Look up connection
	conn, err := r.cfg.ConnectionRepo.GetByID(connectionID)
	if err != nil {
		log.Printf("[webhook] connection not found: %s: %v", connectionID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}

	if conn.PluginID != pluginID {
		log.Printf("[webhook] plugin mismatch: want %s, got %s", pluginID, conn.PluginID)
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}

	if !conn.Enabled {
		log.Printf("[webhook] connection disabled: %s", connectionID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connection disabled"})
		return
	}

	if !conn.WebhookEnabled {
		log.Printf("[webhook] webhooks disabled for connection: %s", connectionID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhooks disabled for this connection"})
		return
	}

	// 2. Read body (limit to 1 MiB to prevent abuse)
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1*1024*1024+1))
	if err != nil {
		log.Printf("[webhook] read body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	if len(body) > 1*1024*1024 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload exceeds 1 MiB"})
		return
	}

	// 3. Resolve webhook secret
	secretRef, ok := conn.CredentialRefs["webhook_secret"]
	if !ok || secretRef == "" {
		log.Printf("[webhook] no webhook_secret configured for connection: %s", connectionID)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "webhook secret not configured"})
		return
	}

	secretPlain, err := secrets.Resolve(r.cfg.Secrets, secretRef)
	if err != nil {
		log.Printf("[webhook] resolve secret: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve webhook secret"})
		return
	}

	// 4. Verify signature
	headers := extractHeaders(c.Request.Header)
	verifier := chooseVerifier(pluginID)
	if err := verifier.Verify(body, secretPlain, headers); err != nil {
		log.Printf("[webhook] signature verification failed for %s/%s: %v", pluginID, connectionID, err)
		r.auditEvent(connectionID, pluginID, "webhook.signature_mismatch", headers, body)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "signature verification failed"})
		return
	}

	// 5. Check event allowlist
	eventType := verifier.EventType(headers, body)
	if len(conn.WebhookEventAllowlist) > 0 {
		allowed := false
		for _, et := range conn.WebhookEventAllowlist {
			if strings.EqualFold(et, eventType) {
				allowed = true
				break
			}
		}
		if !allowed {
			log.Printf("[webhook] event %q not in allowlist for %s", eventType, connectionID)
			c.JSON(http.StatusAccepted, gin.H{"status": "event type not in allowlist, ignored"})
			return
		}
	}

	// 6. Dispatch to plugin
	plugin, err := r.cfg.PluginRegistry.Get(pluginID)
	if err != nil {
		log.Printf("[webhook] plugin not found: %s: %v", pluginID, err)
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}

	receiver, ok := plugin.(plugins.WebhookReceiver)
	if !ok {
		log.Printf("[webhook] plugin %s does not implement WebhookReceiver", pluginID)
		c.JSON(http.StatusNotImplemented, gin.H{"error": "plugin does not support webhooks"})
		return
	}

	ctx := c.Request.Context()
	onFire := func(ctx context.Context, ev plugins.TriggerEvent) error {
		// Resolve the target assistant via trigger bindings.
		bindings, err := r.cfg.BindingRepo.ListByConnection(connectionID)
		if err != nil {
			return fmt.Errorf("list bindings: %w", err)
		}
		var targetAssistant string
		for _, b := range bindings {
			if b.Role == domain.BindingRoleTrigger && b.Enabled {
				targetAssistant = b.AssistantID
				if b.IsPrimary {
					break // primary wins
				}
			}
		}
		if targetAssistant == "" {
			log.Printf("[webhook] no trigger binding for connection %s", connectionID)
			return nil // silently drop — no assistant configured
		}
		_, err = r.cfg.Runtime.CreateRunFromSource(ctx, ev.Goal, targetAssistant, pluginID)
		if err != nil {
			return fmt.Errorf("create run: %w", err)
		}
		return nil
	}

	if err := receiver.ReceiveWebhook(ctx, connectionID, body, headers, onFire); err != nil {
		log.Printf("[webhook] plugin receive failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process webhook"})
		return
	}

	r.auditEvent(connectionID, pluginID, "webhook.delivered", map[string]string{"event_type": eventType}, nil)
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

// handleStatus returns the current tunnel public URL and webhook health.
func (r *Router) handleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) auditEvent(connectionID, pluginID, eventType string, headers map[string]string, body []byte) {
	if r.cfg.EventBus == nil {
		return
	}
	payload := map[string]interface{}{
		"plugin_id":     pluginID,
		"connection_id": connectionID,
		"event_type":    eventType,
		"headers":       headers,
	}
	if len(body) > 0 && len(body) <= 4096 {
		n := len(body)
		if n > 256 {
			n = 256
		}
		payload["body_preview"] = string(body[:n])
	}
	_, _ = r.cfg.EventBus.Publish(context.Background(), domain.EventType(eventType), "", nil, payload)
}

func extractHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
