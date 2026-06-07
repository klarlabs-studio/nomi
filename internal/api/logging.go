package api

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestIDHeader = "X-Request-Id"

// requestIDMiddleware ensures every request has a stable correlation ID.
// If the caller provides X-Request-Id we preserve it; otherwise we generate one.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(requestIDHeader)
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set("request_id", rid)
		c.Writer.Header().Set(requestIDHeader, rid)
		c.Next()
	}
}

// accessLogMiddleware logs only request metadata (method, path, status, latency).
// Request and response bodies are never logged: endpoints like PUT /connectors/:name/config
// and PUT /provider-profiles carry bot tokens and API keys that must not reach stdout/syslog.
func accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		rid := c.GetString("request_id")
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}

		entry := map[string]any{
			"ts":           time.Now().UTC().Format(time.RFC3339Nano),
			"request_id":   rid,
			"method":       c.Request.Method,
			"path":         c.Request.URL.Path,
			"route":        route,
			"status":       c.Writer.Status(),
			"latency_ms":   time.Since(start).Milliseconds(),
			"client_ip":    c.ClientIP(),
			"response_len": c.Writer.Size(),
		}
		b, err := json.Marshal(entry)
		if err != nil {
			log.Printf("access-log-marshal-error request_id=%s err=%v", rid, err)
			return
		}
		log.Printf("%s", b)
	}
}
