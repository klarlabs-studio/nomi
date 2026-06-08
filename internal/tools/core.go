package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.klarlabs.de/nomi/internal/domain"
)

// FileReadTool reads file contents from inside an assistant's workspace root.
type FileReadTool struct{}

// NewFileReadTool creates a new FileReadTool
func NewFileReadTool() *FileReadTool {
	return &FileReadTool{}
}

// Name returns the tool name
func (t *FileReadTool) Name() string {
	return "filesystem.read"
}

// Capability returns the required capability
func (t *FileReadTool) Capability() string {
	return "filesystem.read"
}

// Execute reads a file. The path must resolve inside input["workspace_root"]
// after symlink resolution; without a workspace root the call is refused so a
// misconfigured assistant can't read arbitrary files on the host.
func (t *FileReadTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	rawPath, ok := input["path"].(string)
	if !ok || rawPath == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing file path",
			Message: "Nomi needs a file path to read from. The planner may have forgotten to include it.",
		}
	}
	root, err := WorkspaceRootFromInput(input)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeMissingWorkspace,
			Title:   "No workspace folder",
			Message: "This assistant doesn't have a folder context attached. Add one in the assistant builder so Nomi knows where to read files from.",
			Action:  "Open Assistant Builder",
		}
	}
	resolved, err := ResolveWithinRoot(root, rawPath)
	if err != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePathOutsideRoot,
			Title:   "File outside workspace",
			Message: "The file path goes outside the attached workspace folder. Nomi can only access files inside the workspace.",
		}
	}

	content, err := os.ReadFile(resolved) //nolint:gosec // G304: path resolved within the workspace root
	if err != nil {
		return nil, fmt.Errorf("couldn't read the file: %w", err)
	}

	return map[string]interface{}{
		"content": string(content),
		"path":    resolved,
		"size":    len(content),
	}, nil
}

// FileWriteTool writes content to a file inside an assistant's workspace root.
type FileWriteTool struct{}

// NewFileWriteTool creates a new FileWriteTool
func NewFileWriteTool() *FileWriteTool {
	return &FileWriteTool{}
}

// Name returns the tool name
func (t *FileWriteTool) Name() string {
	return "filesystem.write"
}

// Capability returns the required capability
func (t *FileWriteTool) Capability() string {
	return "filesystem.write"
}

// Execute writes content to the given path. The path must resolve inside
// input["workspace_root"]; the write is refused if no root is declared.
func (t *FileWriteTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	rawPath, ok := input["path"].(string)
	if !ok || rawPath == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing file path",
			Message: "Nomi needs a file path to write to. The planner may have forgotten to include it.",
		}
	}
	content, ok := input["content"].(string)
	if !ok {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing content",
			Message: "Nomi needs content to write to the file. The planner may have forgotten to include it.",
		}
	}
	root, err := WorkspaceRootFromInput(input)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeMissingWorkspace,
			Title:   "No workspace folder",
			Message: "This assistant doesn't have a folder context attached. Add one in the assistant builder so Nomi knows where to write files.",
			Action:  "Open Assistant Builder",
		}
	}
	resolved, err := ResolveWithinRoot(root, rawPath)
	if err != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePathOutsideRoot,
			Title:   "File outside workspace",
			Message: "The file path goes outside the attached workspace folder. Nomi can only write files inside the workspace.",
		}
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o750); err != nil {
		return nil, fmt.Errorf("couldn't create the folder: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("couldn't write the file: %w", err)
	}

	return map[string]interface{}{
		"path":    resolved,
		"size":    len(content),
		"success": true,
	}, nil
}
