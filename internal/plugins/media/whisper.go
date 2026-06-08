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

// WhisperBackend implements STTBackend using the whisper.cpp CLI
// (https://github.com/ggerganov/whisper.cpp) and ggml model files.
//
// Same shape as PiperBackend: probe for the binary at construction,
// soft-nil if absent, shell out per call. Models are user-managed under
// ~/.nomi/models/whisper/ (ggml-<size>.bin). Recommended starting
// model: "base.en" (~140MB, English-only, fast); upgrade to "small"
// or larger for multilingual or higher quality.
type WhisperBackend struct {
	binaryPath string
	modelDir   string
	modelSize  string // "tiny" | "base" | "small" | "medium" | "large" + optional ".en" suffix
}

// whisperBinaryNames lists the binary names whisper.cpp installs as,
// in order of preference. brew installs it as `whisper-cli`; from-source
// builds historically named it `whisper` or `main`. We accept whatever
// the user has on PATH.
var whisperBinaryNames = []string{"whisper-cli", "whisper-cpp", "whisper"}

// NewWhisperBackend probes for one of the whisper.cpp binary names plus
// a model file under modelDir/ggml-<modelSize>.bin. Returns (nil, nil)
// when the binary or model is absent — see PiperBackend's analogous
// soft-nil contract.
//
// modelSize defaults to "base.en". modelDir defaults to ~/.nomi/models/whisper/.
func NewWhisperBackend(modelDir, modelSize string) (*WhisperBackend, error) {
	var binaryPath string
	for _, name := range whisperBinaryNames {
		if path, err := exec.LookPath(name); err == nil {
			binaryPath = path
			break
		}
	}
	if binaryPath == "" {
		return nil, nil
	}
	if modelDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("whisper: user home dir: %w", err)
		}
		modelDir = filepath.Join(home, ".nomi", "models", "whisper")
	}
	if modelSize == "" {
		modelSize = "base.en"
	}
	// Model presence check: bail soft-nil if the model isn't there.
	if _, err := os.Stat(filepath.Join(modelDir, "ggml-"+modelSize+".bin")); err != nil {
		return nil, nil
	}
	return &WhisperBackend{
		binaryPath: binaryPath,
		modelDir:   modelDir,
		modelSize:  modelSize,
	}, nil
}

// Name identifies this backend in tool output.
func (w *WhisperBackend) Name() string { return "whisper.cpp" }

// Transcribe writes audio to a temp file and runs whisper.cpp with
// --output-txt so the transcript lands as a sibling .txt file we can
// read back. languageHint is forwarded to whisper-cli's -l flag for
// accuracy (whisper auto-detects when language="auto"; passing an
// explicit ISO 639-1 code biases the model and is usually faster).
//
// Returns (transcript, detectedLanguage, error). detectedLanguage
// echoes back the languageHint when supplied — whisper-cli writes the
// detected language to stderr in --print-progress mode but parsing
// that is fragile, so we prefer the caller's hint when present.
func (w *WhisperBackend) Transcribe(ctx context.Context, audio []byte, languageHint string) (string, string, error) {
	if len(audio) == 0 {
		return "", "", fmt.Errorf("audio is empty")
	}

	// whisper.cpp historically prefers WAV 16kHz mono; modern builds
	// accept many formats via internal ffmpeg integration. We pass
	// the bytes through unchanged and trust the binary to fail clearly
	// when given an unsupported format. ffmpeg-on-the-fly conversion
	// is a follow-up if users hit format errors in practice.
	audioFile, err := os.CreateTemp("", "whisper-in-*.wav")
	if err != nil {
		return "", "", fmt.Errorf("create temp audio: %w", err)
	}
	audioPath := audioFile.Name()
	defer func() { _ = os.Remove(audioPath) }()
	if _, err := audioFile.Write(audio); err != nil {
		_ = audioFile.Close()
		return "", "", fmt.Errorf("write temp audio: %w", err)
	}
	_ = audioFile.Close()

	// Output base — whisper-cli appends .txt automatically.
	outBase := strings.TrimSuffix(audioPath, ".wav") + "-out"
	outFile := outBase + ".txt"
	defer func() { _ = os.Remove(outFile) }()

	modelPath := filepath.Join(w.modelDir, "ggml-"+w.modelSize+".bin")

	args := []string{
		"-m", modelPath,
		"-f", audioPath,
		"--output-txt",
		"-of", outBase,
		"--no-prints", // suppress stdout chatter; we only want the .txt content
	}
	if languageHint != "" {
		args = append(args, "-l", languageHint)
	}

	// Bound at 5 minutes. STT is slower than TTS — a 1-minute voice
	// note on the "base" model takes ~5-15s on a modern CPU. Capping
	// at 5min handles edge cases (slow CPU, bigger models) without
	// hanging the daemon on a runaway process.
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(runCtx, w.binaryPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("whisper exec: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	transcriptBytes, err := os.ReadFile(outFile)
	if err != nil {
		return "", "", fmt.Errorf("read whisper output: %w", err)
	}
	transcript := strings.TrimSpace(string(transcriptBytes))
	return transcript, languageHint, nil
}
