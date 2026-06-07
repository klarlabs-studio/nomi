package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tunnel"
)

// WebhookServer handles authenticated webhook management endpoints.
type WebhookServer struct {
	connectionRepo *db.ConnectionRepository
	secrets        secrets.Store
	tunnel         tunnel.Adapter
}

// NewWebhookServer creates a webhook management server.
func NewWebhookServer(dbConn *db.DB, secrets secrets.Store, tunnel tunnel.Adapter) *WebhookServer {
	return &WebhookServer{
		connectionRepo: db.NewConnectionRepository(dbConn),
		secrets:        secrets,
		tunnel:         tunnel,
	}
}

// GetTunnelStatus returns the current tunnel public URL and health.
func (s *WebhookServer) GetTunnelStatus(c *gin.Context) {
	url := ""
	if s.tunnel != nil {
		url = s.tunnel.URL()
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":    s.tunnel != nil && url != "",
		"public_url": url,
	})
}

// RotateSecret generates a new webhook secret for a connection.
func (s *WebhookServer) RotateSecret(c *gin.Context) {
	connectionID := c.Param("connection_id")

	conn, err := s.connectionRepo.GetByID(connectionID)
	if err != nil {
		respondNotFound(c, "connection not found")
		return
	}

	// Generate a 32-byte random secret
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		respondInternal(c, "failed to generate secret", err)
		return
	}
	secret := hex.EncodeToString(secretBytes)

	// Store in secrets vault
	key := "webhook_secret_" + connectionID
	ref, err := secrets.StoreAsReference(s.secrets, key, secret)
	if err != nil {
		respondInternal(c, "failed to store secret", err)
		return
	}

	// Update connection credential refs
	if conn.CredentialRefs == nil {
		conn.CredentialRefs = make(map[string]string)
	}
	conn.CredentialRefs["webhook_secret"] = ref
	if err := s.connectionRepo.Update(conn); err != nil {
		respondInternal(c, "failed to update connection", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "secret rotated"})
}

// UpdateAllowlist updates the webhook event allowlist for a connection.
func (s *WebhookServer) UpdateAllowlist(c *gin.Context) {
	connectionID := c.Param("connection_id")

	var req struct {
		Allowlist []string `json:"allowlist"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, "invalid request body")
		return
	}

	conn, err := s.connectionRepo.GetByID(connectionID)
	if err != nil {
		respondNotFound(c, "connection not found")
		return
	}

	conn.WebhookEventAllowlist = req.Allowlist
	if err := s.connectionRepo.Update(conn); err != nil {
		respondInternal(c, "failed to update connection", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"allowlist": req.Allowlist})
}
