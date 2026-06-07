package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// Inbound attachment enrichment pass (ADR 0001 §rich-media, task
// media-10). Runs before the planner so the assistant sees a goal
// string that already includes transcripts of voice notes, summaries
// of attached docs, etc. Without this pass the planner would only see
// the raw caption (or "(voice note, no caption)") and have no idea
// what the user actually said.
//
// Today the pass handles audio → media.transcribe. Image and document
// enrichment will land alongside their backends; the dispatch table
// is structured so adding a kind is a one-liner.

// EnrichmentService walks a Run's captured RunAttachments and asks the
// configured media tools to extract textual content from them. The
// extracted text is folded into the planner input so the assistant
// reasons over a goal that includes the attachment content.
//
// Backends bind via the existing tools.Registry — the service doesn't
// know about Piper or whisper.cpp directly, only that "media.transcribe"
// is a registered tool. When the tool is missing (no Media plugin
// installed, or the user hasn't configured backends), enrichment
// silently skips and the planner sees the original goal.
type EnrichmentService struct {
	attachmentRepo *db.RunAttachmentRepository
	toolExecutor   *tools.Executor
	httpClient     *http.Client
}

// NewEnrichmentService wires the dependencies. httpClient defaults to
// a 30s-timeout client when nil — the fetch step has to be bounded
// because users can paste enormous documents that would otherwise
// stall the entire planning phase.
func NewEnrichmentService(repo *db.RunAttachmentRepository, executor *tools.Executor, client *http.Client) *EnrichmentService {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &EnrichmentService{
		attachmentRepo: repo,
		toolExecutor:   executor,
		httpClient:     client,
	}
}

// Enrich returns a goal string augmented with attachment-derived
// content, or the original goal unchanged when there's nothing to
// add. Errors during individual attachment fetch/transcribe are
// non-fatal — they're suppressed so the planner can still proceed
// with whatever extraction succeeded. Returning an error here would
// strand the entire Run on a single malformed attachment.
func (s *EnrichmentService) Enrich(ctx context.Context, runID, originalGoal string) string {
	if s.attachmentRepo == nil || s.toolExecutor == nil {
		return originalGoal
	}
	atts, err := s.attachmentRepo.ListByRun(runID)
	if err != nil || len(atts) == 0 {
		return originalGoal
	}
	var enriched []string
	for _, att := range atts {
		extracted := s.extractContent(ctx, att)
		if extracted != "" {
			enriched = append(enriched, extracted)
		}
	}
	if len(enriched) == 0 {
		return originalGoal
	}
	return originalGoal + "\n\n" + strings.Join(enriched, "\n\n")
}

// extractContent dispatches to the right tool per attachment kind.
// Returns "" when extraction wasn't possible (no URL, tool missing,
// fetch failed, transcribe failed, etc).
func (s *EnrichmentService) extractContent(ctx context.Context, att *domain.RunAttachment) string {
	switch att.Kind {
	case "audio":
		return s.transcribeAudio(ctx, att)
	case "image":
		// Vision enrichment lands alongside a vision backend; for now
		// we just announce the attachment so the planner knows it's
		// there.
		return fmt.Sprintf("[image attachment: %s (%s)]", att.Filename, att.ContentType)
	case "document":
		return fmt.Sprintf("[document attachment: %s (%s)]", att.Filename, att.ContentType)
	case "video":
		return fmt.Sprintf("[video attachment: %s (%s)]", att.Filename, att.ContentType)
	}
	return ""
}

// transcribeAudio fetches the audio bytes and dispatches media.transcribe.
// The URL must already be resolvable — Telegram-only attachments stored
// with external_id but no URL are skipped (a follow-up "Telegram getFile
// resolver" would convert file_id → URL on demand).
func (s *EnrichmentService) transcribeAudio(ctx context.Context, att *domain.RunAttachment) string {
	if att.URL == "" {
		return ""
	}
	audio, err := s.fetchBytes(ctx, att.URL)
	if err != nil || len(audio) == 0 {
		return ""
	}
	result := s.toolExecutor.Execute(ctx, "media.transcribe", map[string]interface{}{
		"audio": audio,
	})
	if !result.Success {
		return ""
	}
	transcript, _ := result.Output["transcript"].(string)
	if transcript == "" {
		return ""
	}
	label := att.Filename
	if label == "" {
		label = "voice"
	}
	return fmt.Sprintf("[voice transcript: %s]\n%s", label, transcript)
}

// fetchBytes is a small bounded GET — bounded by the httpClient's
// timeout and a hard 25 MiB ceiling on response body size to keep one
// runaway attachment from exhausting the daemon's memory.
func (s *EnrichmentService) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	const maxBytes = 25 * 1024 * 1024
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}
