package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

type AuditServer struct {
	events    *db.EventRepository
	approvals *db.ApprovalRepository
	settings  *db.AppSettingsRepository
	pubKey    ed25519.PublicKey
	privKey   ed25519.PrivateKey
}

func NewAuditServer(database *db.DB, authToken string) *AuditServer {
	seed := sha256.Sum256([]byte(authToken))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	return &AuditServer{
		events:    db.NewEventRepository(database),
		approvals: db.NewApprovalRepository(database),
		settings:  db.NewAppSettingsRepository(database),
		pubKey:    pub,
		privKey:   priv,
	}
}

type auditEnvelope struct {
	From      string                         `json:"from"`
	To        string                         `json:"to"`
	Generated string                         `json:"generated_at"`
	Format    string                         `json:"format"`
	Redacted  bool                           `json:"redacted"`
	PublicKey string                         `json:"public_key"`
	Events    []*domain.Event                `json:"events"`
	Approvals []*permissions.ApprovalRequest `json:"approvals"`
	Signature string                         `json:"signature,omitempty"`
	Algorithm string                         `json:"algorithm,omitempty"`
}

func (s *AuditServer) Export(c *gin.Context) {
	from, err := time.Parse(time.RFC3339, c.Query("from"))
	if err != nil {
		respondValidationError(c, "invalid from; use RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, c.Query("to"))
	if err != nil {
		respondValidationError(c, "invalid to; use RFC3339")
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "json")))
	if format != "json" && format != "ndjson" {
		respondValidationError(c, "invalid format; use json|ndjson")
		return
	}
	redacted := c.DefaultQuery("redact", "false") == "true"

	eventsList, err := s.events.ListByTimeRange(from, to)
	if err != nil {
		respondInternal(c, "audit operation failed", err)
		return
	}
	approvals, err := s.approvals.ListByTimeRange(from, to)
	if err != nil {
		respondInternal(c, "audit operation failed", err)
		return
	}

	if redacted {
		for _, e := range eventsList {
			e.Payload = redactMap(e.Payload)
		}
		for _, a := range approvals {
			a.Context = redactMap(a.Context)
		}
	}

	env := auditEnvelope{
		From:      from.UTC().Format(time.RFC3339),
		To:        to.UTC().Format(time.RFC3339),
		Generated: time.Now().UTC().Format(time.RFC3339),
		Format:    format,
		Redacted:  redacted,
		PublicKey: base64.StdEncoding.EncodeToString(s.pubKey),
		Events:    eventsList,
		Approvals: approvals,
		Algorithm: "ed25519",
	}

	if format == "json" {
		payload, err := json.Marshal(env)
		if err != nil {
			respondInternal(c, "audit operation failed", err)
			return
		}
		env.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.privKey, payload))
		c.JSON(http.StatusOK, env)
		return
	}

	ndjson, sig, err := s.buildNDJSON(env)
	if err != nil {
		respondInternal(c, "audit operation failed", err)
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	_, _ = c.Writer.Write(ndjson)
	_, _ = c.Writer.Write([]byte("\n"))
	_, _ = c.Writer.Write([]byte(sig))
}

type pruneAuditRequest struct {
	Days int `json:"days" binding:"required"`
}

func (s *AuditServer) Prune(c *gin.Context) {
	var req pruneAuditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if req.Days <= 0 {
		respondValidationError(c, "days must be >0")
		return
	}
	if req.Days <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "days must be > 0"})
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(req.Days) * 24 * time.Hour)
	deleted, err := s.events.DeleteOlderThan(cutoff)
	if err != nil {
		respondInternal(c, "audit operation failed", err)
		return
	}
	_ = s.settings.Set("audit_retention_days", strconv.Itoa(req.Days))
	c.JSON(http.StatusOK, gin.H{
		"status":         "pruned",
		"deleted":        deleted,
		"cutoff":         cutoff.Format(time.RFC3339),
		"retention_days": req.Days,
	})
}

func (s *AuditServer) buildNDJSON(env auditEnvelope) ([]byte, string, error) {
	buf := bytes.NewBuffer(nil)
	meta := map[string]any{
		"type":         "meta",
		"from":         env.From,
		"to":           env.To,
		"generated_at": env.Generated,
		"redacted":     env.Redacted,
		"public_key":   env.PublicKey,
		"algorithm":    env.Algorithm,
	}
	metaLine, err := json.Marshal(meta)
	if err != nil {
		return nil, "", err
	}
	buf.Write(metaLine)

	for _, e := range env.Events {
		line, err := json.Marshal(map[string]any{"type": "event", "event": e})
		if err != nil {
			return nil, "", err
		}
		buf.WriteByte('\n')
		buf.Write(line)
	}
	for _, a := range env.Approvals {
		line, err := json.Marshal(map[string]any{"type": "approval", "approval": a})
		if err != nil {
			return nil, "", err
		}
		buf.WriteByte('\n')
		buf.Write(line)
	}

	sig := ed25519.Sign(s.privKey, buf.Bytes())
	sigLine, err := json.Marshal(map[string]any{
		"type":       "signature",
		"algorithm":  "ed25519",
		"signature":  base64.StdEncoding.EncodeToString(sig),
		"public_key": env.PublicKey,
	})
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), string(sigLine), nil
}

func redactMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "content") || strings.Contains(lower, "input") || strings.Contains(lower, "output") {
			out[k] = "sha256:" + hashAny(v)
			continue
		}
		switch typed := v.(type) {
		case map[string]interface{}:
			out[k] = redactMap(typed)
		default:
			out[k] = v
		}
	}
	return out
}

func hashAny(v interface{}) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
