package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// ConversationServer exposes the persistent-thread entity added by ADR
// 0001 §8. Endpoints are intentionally read-mostly — conversations are
// created as a side effect of channel plugins receiving inbound messages,
// not by explicit REST calls. The only mutating operation here is
// deletion (for user housekeeping).
type ConversationServer struct {
	convs *db.ConversationRepository
	runs  *db.RunRepository
}

// NewConversationServer constructs the conversation endpoint handler.
func NewConversationServer(convs *db.ConversationRepository, runs *db.RunRepository) *ConversationServer {
	return &ConversationServer{convs: convs, runs: runs}
}

// ListConversations returns every conversation for a given assistant, or
// a given connection, ordered by most recently active. The UI's Chats
// tab consumes this to render the thread list.
//
// Query params (at least one required):
//
//	?assistant_id=ID
//	?connection_id=ID
func (s *ConversationServer) ListConversations(c *gin.Context) {
	assistantID := c.Query("assistant_id")
	connectionID := c.Query("connection_id")
	if assistantID == "" && connectionID == "" {
		respondValidationError(c, "assistant_id or connection_id is required")
		return
	}
	if assistantID != "" {
		list, err := s.convs.ListByAssistant(assistantID, 100)
		if err != nil {
			respondInternal(c, "failed to list conversations", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"conversations": list})
		return
	}
	list, err := s.convs.ListByConnection(connectionID, 100)
	if err != nil {
		respondInternal(c, "failed to list conversations", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"conversations": list})
}

// GetConversation returns one conversation by id.
func (s *ConversationServer) GetConversation(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	conv, err := s.convs.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, conv)
}

// DeleteConversation removes a conversation. Linked runs survive with
// their conversation_id set to NULL (via ON DELETE SET NULL on the FK)
// so historical execution records aren't lost.
func (s *ConversationServer) DeleteConversation(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	if err := s.convs.Delete(id, nil); err != nil {
		respondInternal(c, "failed to delete conversation", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
