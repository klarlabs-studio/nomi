// Package media implements the local-first media plugin: TTS, STT, and
// vision tools backed by open-source models that run on the user's
// machine. Cardinality is single — one install of the plugin lights up
// rich-media capabilities across every channel that supports them via
// the OutboundMessage / RunAttachment contract (ADR 0001 §rich-media).
//
// Backends are pluggable through the Backend interface so we can
// support multiple TTS implementations (Piper today; XTTS / Elevenlabs
// later if a user opts into a cloud API). The plugin ships with the
// Piper TTS + whisper.cpp STT backends wired in; assistants that
// invoke a tool without a configured backend get a clear error rather
// than a silent no-op.
package media

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

// PluginID is the stable reverse-DNS identifier for the media plugin.
const PluginID = "com.nomi.media"

// Plugin is the media plugin. Tool-only; no channel role, no triggers
// in v1 (though future "fire when whisper detects keyword X" triggers
// could slot in).
type Plugin struct {
	mu sync.RWMutex

	// running is the standard PluginStatus.Running flag. The plugin
	// itself has no long-running worker — backends spawn subprocesses
	// per tool call and exit.
	running bool

	// tts is the bound TTS backend. Nil when no backend is configured;
	// media.speak then returns a clear "not configured" error rather
	// than silently failing.
	tts TTSBackend
	// stt is the bound STT backend. Same nil-tolerance as tts.
	stt STTBackend
}

// TTSBackend is the contract every text-to-speech implementation
// implements. Returns audio bytes (typically Ogg Opus for Telegram
// compatibility) plus the content type. voice is an optional
// backend-specific voice identifier (e.g. Piper's "en_US-amy-medium").
type TTSBackend interface {
	Name() string
	Speak(ctx context.Context, text, voice string) (audio []byte, contentType string, err error)
}

// STTBackend is the contract for speech-to-text. Returns the transcript
// plus an optional detected-language code. languageHint is an optional
// ISO 639-1 hint the backend may use to bias detection.
type STTBackend interface {
	Name() string
	Transcribe(ctx context.Context, audio []byte, languageHint string) (transcript, detectedLanguage string, err error)
}

// NewPlugin constructs the Media plugin. Backends are bound separately
// via SetTTSBackend / SetSTTBackend so main() can wire backends only
// when the binaries are present on the host — the plugin still
// registers + manifests when no backend is around, and the tool calls
// surface a clean "not configured" error.
func NewPlugin() *Plugin {
	return &Plugin{}
}

// SetTTSBackend installs (or replaces) the TTS implementation.
func (p *Plugin) SetTTSBackend(b TTSBackend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tts = b
}

// SetSTTBackend installs (or replaces) the STT implementation.
func (p *Plugin) SetSTTBackend(b STTBackend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stt = b
}

// Manifest declares the media plugin's contract. All tools are
// connection-less (cardinality single) — no per-account concept; the
// configured backend is the same regardless of which assistant invokes
// the tool.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Media",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Local-first text-to-speech, speech-to-text, and image description via open-source models. One install, all channels benefit.",
		Cardinality: plugins.ConnectionSingle,
		Capabilities: []string{
			"media.tts",
			"media.stt",
			// media.vision capability stays declared as a ceiling so
			// assistants can be configured for it now; the actual
			// describe_image tool ships with the LLaVA backend in v0.2.
			"media.vision",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{
					Name:        "media.speak",
					Capability:  "media.tts",
					Description: "Convert text to speech audio bytes. Inputs: text, voice? (backend-specific id). Returns: audio (base64), content_type.",
				},
				{
					Name:        "media.transcribe",
					Capability:  "media.stt",
					Description: "Convert speech audio bytes to text. Inputs: audio (base64) or url, language_hint? (ISO 639-1). Returns: transcript, detected_language.",
				},
				// media.describe_image (capability media.vision) intentionally
				// not advertised: the vision backend wiring isn't in V1 and
				// surfacing a tool that always errors confuses planners. It
				// reappears alongside the LLaVA-via-Ollama backend in v0.2.
			},
		},
		Requires: plugins.Requirements{
			ConfigSchema: map[string]plugins.ConfigField{
				"tts_backend": {
					Type: "string", Label: "TTS backend",
					Default:     "piper",
					Description: `Which text-to-speech backend to use: "piper" (default, local) or "none" to disable.`,
				},
				"tts_voice": {
					Type: "string", Label: "Default TTS voice",
					Default:     "en_US-amy-medium",
					Description: "Backend-specific voice identifier. Piper voices live under ~/.nomi/models/piper/.",
				},
				"stt_backend": {
					Type: "string", Label: "STT backend",
					Default:     "whisper",
					Description: `Which speech-to-text backend to use: "whisper" (default, local) or "none" to disable.`,
				},
				"stt_model": {
					Type: "string", Label: "Whisper model size",
					Default:     "base",
					Description: `Whisper model: "tiny" / "base" / "small" / "medium" / "large". Larger = more accurate, slower, larger download.`,
				},
			},
		},
	}
}

// Configure is a no-op today — backend selection is done at boot via
// SetTTSBackend / SetSTTBackend rather than per-Configure invocation.
// Future per-connection-style runtime config would land here.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start marks the plugin running. No background worker — tool calls
// dispatch backends synchronously.
func (p *Plugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop unwinds the running flag.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// Status returns plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// Tools implements plugins.ToolProvider. Each tool routes to its
// backend (or returns a "backend not configured" error when the
// corresponding Set*Backend wasn't called).
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&speakTool{plugin: p},
		&transcribeTool{plugin: p},
	}
}

// --- speak tool ---

type speakTool struct{ plugin *Plugin }

func (t *speakTool) Name() string       { return "media.speak" }
func (t *speakTool) Capability() string { return "media.tts" }

func (t *speakTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("media.speak: text is required")
	}
	voice, _ := input["voice"].(string)
	t.plugin.mu.RLock()
	backend := t.plugin.tts
	t.plugin.mu.RUnlock()
	if backend == nil {
		return nil, fmt.Errorf("media.speak: no TTS backend configured (install Piper and wire it via SetTTSBackend)")
	}
	audio, contentType, err := backend.Speak(ctx, text, voice)
	if err != nil {
		return nil, fmt.Errorf("media.speak: %w", err)
	}
	return map[string]interface{}{
		"audio":        audio,
		"content_type": contentType,
		"backend":      backend.Name(),
	}, nil
}

// --- transcribe tool ---

type transcribeTool struct{ plugin *Plugin }

func (t *transcribeTool) Name() string       { return "media.transcribe" }
func (t *transcribeTool) Capability() string { return "media.stt" }

func (t *transcribeTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	audio, ok := input["audio"].([]byte)
	if !ok || len(audio) == 0 {
		return nil, fmt.Errorf("media.transcribe: audio bytes are required (URL fetching lands in media-09)")
	}
	languageHint, _ := input["language_hint"].(string)
	t.plugin.mu.RLock()
	backend := t.plugin.stt
	t.plugin.mu.RUnlock()
	if backend == nil {
		return nil, fmt.Errorf("media.transcribe: no STT backend configured (install whisper.cpp and wire it via SetSTTBackend)")
	}
	transcript, lang, err := backend.Transcribe(ctx, audio, languageHint)
	if err != nil {
		return nil, fmt.Errorf("media.transcribe: %w", err)
	}
	return map[string]interface{}{
		"transcript":        transcript,
		"detected_language": lang,
		"backend":           backend.Name(),
	}, nil
}

// Compile-time guards.
var _ plugins.Plugin = (*Plugin)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ tools.Tool = (*speakTool)(nil)
var _ tools.Tool = (*transcribeTool)(nil)
