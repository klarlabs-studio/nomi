package api

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

// EventServer handles event-related endpoints
type EventServer struct {
	bus *events.EventBus
}

// NewEventServer creates a new event server
func NewEventServer(bus *events.EventBus) *EventServer {
	return &EventServer{bus: bus}
}

// ListEvents lists events with optional run_id, type, and limit filtering.
func (s *EventServer) ListEvents(c *gin.Context) {
	runID := c.Query("run_id")
	typeFilter := c.Query("type")
	limitStr := c.DefaultQuery("limit", "100")

	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 0 {
		respondValidationError(c, "invalid limit")
		return
	}

	var eventList []*domain.Event
	if runID != "" {
		eventList, err = s.bus.GetHistory(runID, limit)
	} else {
		eventList, err = s.bus.GetAllHistory(limit)
	}
	if err != nil {
		respondInternal(c, "failed to list events", err)
		return
	}

	if typeFilter != "" {
		filtered := make([]*domain.Event, 0, len(eventList))
		for _, e := range eventList {
			if string(e.Type) == typeFilter {
				filtered = append(filtered, e)
			}
		}
		eventList = filtered
	}

	c.JSON(http.StatusOK, gin.H{"events": eventList})
}

// sseHeartbeatInterval keeps intermediaries (Tauri's reqwest timeouts, kernel
// TCP keep-alive windows) from thinking the SSE connection is dead. The Rust
// side ignores comment lines, so this is invisible to callers.
const sseHeartbeatInterval = 15 * time.Second

// StreamEvents streams events via Server-Sent Events with a periodic heartbeat.
func (s *EventServer) StreamEvents(c *gin.Context) {
	runID := c.Query("run_id")

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	filter := events.EventFilter{}
	if runID != "" {
		filter.RunID = &runID
	}

	sub := s.bus.Subscribe(filter)
	defer sub.Unsubscribe()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Flush an initial comment so clients see headers + 200 immediately
	// instead of blocking until the first real event. Some HTTP clients
	// (and intermediaries) buffer until at least one body byte appears.
	_, _ = io.WriteString(c.Writer, ": ready\n\n")
	c.Writer.Flush()

	c.Stream(func(w io.Writer) bool {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				return false
			}
			c.SSEvent("message", event)
			return true
		case <-heartbeat.C:
			// Comment lines are part of the SSE spec and serve as keepalives.
			_, err := io.WriteString(w, ": ping\n\n")
			return err == nil
		case <-c.Request.Context().Done():
			return false
		}
	})
}
