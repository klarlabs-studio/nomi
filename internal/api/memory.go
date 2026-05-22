package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/felixgeelhaar/mnemos"
	"github.com/felixgeelhaar/mnemos/embedded"
)

// MemoryServer handles memory-related endpoints. All operations go
// through mnemos.Client; the legacy *memory.Manager path is gone, so
// REST CRUD reads/writes the same store the runtime writes to.
type MemoryServer struct {
	client mnemos.Client
}

// NewMemoryServer creates a new memory server.
func NewMemoryServer(client mnemos.Client) *MemoryServer {
	return &MemoryServer{client: client}
}

// idLookupScopes lists the scopes the ID-based handlers iterate when
// the client doesn't carry a scope in the path. ID is globally unique
// (uuid) so at most one row matches; the order here is the search
// priority. Workspace first since that's where 90%+ of writes land.
var idLookupScopes = []mnemos.Scope{
	mnemos.LocalWorkspace(),
	mnemos.LocalProfile(),
	mnemos.LocalPreferences(),
}

// CreateMemoryRequest represents a request to create a memory entry.
type CreateMemoryRequest struct {
	Content     string  `json:"content" binding:"required"`
	Scope       string  `json:"scope"`
	AssistantID *string `json:"assistant_id,omitempty"`
	RunID       *string `json:"run_id,omitempty"`
}

// memoryResponse is the JSON shape the API returns for an entry.
// Keeps the wire-format independent of mnemos.Entry's exact field
// names should they evolve upstream.
type memoryResponse struct {
	ID          string  `json:"id"`
	Scope       string  `json:"scope"`
	Content     string  `json:"content"`
	AssistantID *string `json:"assistant_id,omitempty"`
	RunID       *string `json:"run_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
	ContentHash string  `json:"content_hash,omitempty"`
}

func toResponse(scope mnemos.ScopeKind, e *mnemos.Entry) memoryResponse {
	return memoryResponse{
		ID:          e.ID,
		Scope:       string(scope),
		Content:     e.Content,
		AssistantID: e.AssistantID,
		RunID:       e.RunID,
		CreatedAt:   e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		ContentHash: e.ContentHash,
	}
}

// scopeFromString maps the request's "scope" string into a mnemos.Scope
// using the embedded-mode defaults. Empty input defaults to workspace.
func scopeFromString(s string) (mnemos.Scope, error) {
	if s == "" {
		return mnemos.LocalWorkspace(), nil
	}
	kind := mnemos.ScopeKind(s)
	if !kind.IsValid() {
		return mnemos.Scope{}, fmt.Errorf("invalid scope %q", s)
	}
	switch kind {
	case mnemos.ScopeWorkspace:
		return mnemos.LocalWorkspace(), nil
	case mnemos.ScopeProfile:
		return mnemos.LocalProfile(), nil
	case mnemos.ScopePreferences:
		return mnemos.LocalPreferences(), nil
	default:
		return mnemos.Scope{OwnerID: mnemos.LocalOwnerID, Kind: kind, Key: mnemos.DefaultWorkspaceKey}, nil
	}
}

// CreateMemory creates a new memory entry.
func (s *MemoryServer) CreateMemory(c *gin.Context) {
	var req CreateMemoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	scope, err := scopeFromString(req.Scope)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}

	entry := &mnemos.Entry{
		Content:     req.Content,
		AssistantID: req.AssistantID,
		RunID:       req.RunID,
	}
	if err := s.client.Store(c.Request.Context(), scope, entry); err != nil {
		respondInternal(c, "failed to create memory", err)
		return
	}
	c.JSON(http.StatusCreated, toResponse(scope.Kind, entry))
}

// ListMemory lists memory entries with optional filtering. Query
// params:
//
//   scope — workspace | profile | preferences (default: workspace)
//   q     — case-insensitive substring filter; routes to Search
//   limit — page size (default 100)
//
// When no scope is supplied the handler defaults to workspace (the
// previous "union workspace + profile" behavior is dropped — callers
// who want multiple scopes pass them explicitly).
func (s *MemoryServer) ListMemory(c *gin.Context) {
	scope, err := scopeFromString(c.Query("scope"))
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	query := c.Query("q")
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil {
		respondValidationError(c, "invalid limit")
		return
	}

	var entries []*mnemos.Entry
	if query != "" {
		entries, err = s.client.Search(c.Request.Context(), scope, query, mnemos.SearchOpts{Limit: limit})
	} else {
		entries, err = s.client.Retrieve(c.Request.Context(), scope, mnemos.Query{Limit: limit})
	}
	if err != nil {
		respondInternal(c, "failed to list memories", err)
		return
	}

	out := make([]memoryResponse, len(entries))
	for i, e := range entries {
		out[i] = toResponse(scope.Kind, e)
	}
	c.JSON(http.StatusOK, gin.H{"memories": out})
}

// GetMemory retrieves a memory entry by ID. ID-based endpoints iterate
// scope candidates because the path doesn't carry a scope hint.
func (s *MemoryServer) GetMemory(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	for _, scope := range idLookupScopes {
		entry, err := s.client.GetByID(c.Request.Context(), scope, id)
		if err == nil {
			c.JSON(http.StatusOK, toResponse(scope.Kind, entry))
			return
		}
		if !errors.Is(err, mnemos.ErrNotFound) {
			respondInternal(c, "failed to get memory", err)
			return
		}
	}
	respondNotFound(c, "memory entry not found")
}

// DeleteMemory deletes a memory entry. Iterates scopes the same way as
// GetMemory; deletes from whichever scope hits first.
func (s *MemoryServer) DeleteMemory(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	for _, scope := range idLookupScopes {
		err := s.client.Forget(c.Request.Context(), scope, id)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"status": "deleted"})
			return
		}
		if !errors.Is(err, mnemos.ErrNotFound) {
			respondInternal(c, "failed to delete memory", err)
			return
		}
	}
	respondNotFound(c, "memory entry not found")
}

// ExportMemory streams a Mnemos JSONL export of the requested scope.
// Query params:
//
//   scope — workspace | profile | preferences (default: workspace)
//   key   — workspace key (default: "default" for workspace; empty for
//           profile/preferences which take no key)
//
// Content-Type is application/x-ndjson; the first line is the export
// header, subsequent lines are entries.
func (s *MemoryServer) ExportMemory(c *gin.Context) {
	scope, err := parseScopeQuery(c)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="mnemos-%s.jsonl"`, scope.Kind))
	if _, err := embedded.Export(c.Request.Context(), s.client, scope, c.Writer); err != nil {
		// Body may already be partly written; can't switch status code.
		_, _ = fmt.Fprintf(c.Writer, "\n# export error: %s\n", err.Error())
		return
	}
}

// ImportMemory reads a Mnemos JSONL stream from the request body and
// stores every entry through the configured client. Returns the count
// imported. Body must start with a valid header line; mismatched
// format/version is a 400.
func (s *MemoryServer) ImportMemory(c *gin.Context) {
	n, err := embedded.Import(c.Request.Context(), s.client, c.Request.Body)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"imported": n})
}

// parseScopeQuery builds a mnemos.Scope from the request's scope/key
// query params, applying the embedded-mode defaults.
func parseScopeQuery(c *gin.Context) (mnemos.Scope, error) {
	kindStr := c.DefaultQuery("scope", string(mnemos.ScopeWorkspace))
	kind := mnemos.ScopeKind(kindStr)
	if !kind.IsValid() {
		return mnemos.Scope{}, fmt.Errorf("invalid scope %q", kindStr)
	}
	key := c.Query("key")
	scope := mnemos.Scope{OwnerID: mnemos.LocalOwnerID, Kind: kind, Key: key}
	switch kind {
	case mnemos.ScopeWorkspace, mnemos.ScopeSession, mnemos.ScopeOrg:
		if scope.Key == "" {
			scope.Key = mnemos.DefaultWorkspaceKey
		}
	case mnemos.ScopeProfile, mnemos.ScopePreferences:
		scope.Key = ""
	}
	if err := mnemos.ValidateScope(scope); err != nil {
		return mnemos.Scope{}, err
	}
	return scope, nil
}
