package media

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PiperBackend implements TTSBackend using the Piper CLI binary
// (https://github.com/rhasspy/piper) and ONNX voice models.
//
// Why Piper for v1: it's the industry-standard local TTS — small
// binary (~50MB), per-voice model files (~50MB each, downloaded once),
// runs fast on CPU, no GPU required. Quality is good enough that
// non-techies don't perceive it as robotic.
//
// Discovery model: we don't bundle Piper. Users install it via their
// package manager (`brew install piper-tts` on macOS, prebuilt
// releases on Linux) and drop voice models into ~/.nomi/models/piper/.
// At plugin boot time we look for the binary on PATH and the models
// in the expected directory; if both are present, the backend
// activates and media.speak becomes callable. Otherwise the plugin
// stays manifested but media.speak returns "no TTS backend
// configured" (handled in the plugin layer).
type PiperBackend struct {
	// binaryPath is the absolute path to the piper executable, resolved
	// once at construction so we don't shell out to look it up on
	// every call.
	binaryPath string
	// modelDir is where voice model files (*.onnx + *.onnx.json) live.
	// Defaults to ~/.nomi/models/piper/.
	modelDir string
	// defaultVoice names the model to use when the caller doesn't pass
	// one to Speak (matches Piper's --model arg, sans the .onnx extension).
	defaultVoice string
}

// NewPiperBackend probes the host for piper + at least one voice
// model and returns a ready backend, or (nil, nil) when Piper isn't
// installed — that's a normal "feature not configured" state, not an
// error. Returns (nil, err) only for unexpected I/O failures during
// the probe.
//
// modelDir is the absolute path to the voice-model directory; pass
// "" to default to ~/.nomi/models/piper/. defaultVoice is the model
// name (without .onnx) to use when callers don't specify one.
func NewPiperBackend(modelDir, defaultVoice string) (*PiperBackend, error) {
	binaryPath, err := exec.LookPath("piper")
	if err != nil {
		// Not installed; return a soft-nil so main() can still register
		// the media plugin without failing.
		return nil, nil
	}
	if modelDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("piper: user home dir: %w", err)
		}
		modelDir = filepath.Join(home, ".nomi", "models", "piper")
	}
	if defaultVoice == "" {
		defaultVoice = "en_US-amy-medium"
	}
	// Soft-check that the model directory exists. If it doesn't yet
	// (user hasn't downloaded models), return nil so the plugin shows
	// as unconfigured rather than crashing on first speak.
	if _, err := os.Stat(modelDir); err != nil {
		return nil, nil
	}
	return &PiperBackend{
		binaryPath:   binaryPath,
		modelDir:     modelDir,
		defaultVoice: defaultVoice,
	}, nil
}

// Name identifies this backend in tool output for diagnostics.
func (p *PiperBackend) Name() string { return "piper" }

// Speak runs `piper --model <voice>.onnx --output_file <tmp>` with the
// text on stdin, then reads the WAV bytes back. Output is WAV (Piper's
// default container) — channel adapters that need OGG (Telegram voice
// messages) can transcode via ffmpeg in a follow-up; for v1 WAV is
// portable enough that every channel can render it as a regular audio
// attachment.
//
// voice may be "" — falls back to the configured defaultVoice. Voice
// names map to file names: "en_US-amy-medium" → en_US-amy-medium.onnx
// in modelDir.
func (p *PiperBackend) Speak(ctx context.Context, text, voice string) ([]byte, string, error) {
	if text == "" {
		return nil, "", fmt.Errorf("text is empty")
	}
	if voice == "" {
		voice = p.defaultVoice
	}
	// Reject path traversal in the voice id so a malicious caller
	// can't escape modelDir. Voice ids should be plain identifiers.
	if strings.ContainsAny(voice, "/\\.") {
		return nil, "", fmt.Errorf("invalid voice id %q (no slashes or dots allowed)", voice)
	}
	modelPath := filepath.Join(p.modelDir, voice+".onnx")
	if _, err := os.Stat(modelPath); err != nil {
		return nil, "", fmt.Errorf("voice model %q not found in %s", voice, p.modelDir)
	}

	tmpFile, err := os.CreateTemp("", "piper-*.wav")
	if err != nil {
		return nil, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	// Bound the subprocess: 30s upper bound on a single utterance is
	// generous (Piper does ~5x realtime on a modern CPU; a 30s budget
	// covers a multi-paragraph reply). Cancellation flows through
	// CommandContext so the parent ctx kill propagates.
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, p.binaryPath, //nolint:gosec // G204: piper binary path from plugin config
		"--model", modelPath,
		"--output_file", tmpPath,
	)
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("piper exec: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	audio, err := os.ReadFile(tmpPath) //nolint:gosec // G304: self-created temp output file
	if err != nil {
		return nil, "", fmt.Errorf("read piper output: %w", err)
	}
	return audio, "audio/wav", nil
}
