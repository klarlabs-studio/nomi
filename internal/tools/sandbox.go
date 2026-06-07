package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kballard/go-shellquote"
	"go.klarlabs.de/nomi/internal/domain"
)

// ErrPathEscapesRoot is returned when a user-supplied path resolves outside its
// workspace root (after symlink resolution).
var ErrPathEscapesRoot = errors.New("path escapes workspace root")

// ErrMissingRoot is returned when a tool that requires a workspace root is
// invoked without one. Callers (typically the runtime) are responsible for
// injecting the root based on the assistant's declared contexts; a missing
// root means the assistant has no attached workspace and write/traverse
// operations must be refused.
var ErrMissingRoot = errors.New("workspace_root is required; attach a folder context to the assistant")

// ResolveWithinRoot validates that the user-supplied path resolves to a
// location inside the given root and returns the canonical absolute path.
// Both root and path are resolved through EvalSymlinks so a symlink leading
// outside the root is refused.
//
// Relative user paths are resolved against root; absolute paths are accepted
// only if they stay inside the (resolved) root.
//
// If the path does not yet exist (e.g. a write target), the check is applied
// to its parent directory, which must already exist inside root.
func ResolveWithinRoot(root, userPath string) (string, error) {
	if root == "" {
		return "", ErrMissingRoot
	}
	if userPath == "" {
		return "", fmt.Errorf("path is required")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("invalid root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("root does not exist or is inaccessible: %w", err)
	}

	candidate := userPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(resolvedRoot, candidate)
	}
	candidate = filepath.Clean(candidate)

	// For paths that exist, resolve symlinks and compare. For paths that
	// don't exist (e.g. a new file to write), anchor the check to the
	// deepest ancestor that does exist.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to resolve path: %w", err)
		}
		// Walk up until we find an existing ancestor.
		ancestor := candidate
		for {
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return "", fmt.Errorf("no existing ancestor for %s", userPath)
			}
			ancestor = parent
			resolvedAncestor, ancErr := filepath.EvalSymlinks(ancestor)
			if ancErr == nil {
				// Reconstruct by prefixing the resolved ancestor back onto
				// the unresolved remainder.
				rel, relErr := filepath.Rel(ancestor, candidate)
				if relErr != nil {
					return "", fmt.Errorf("failed to resolve relative: %w", relErr)
				}
				resolved = filepath.Join(resolvedAncestor, rel)
				break
			}
			if !os.IsNotExist(ancErr) {
				return "", fmt.Errorf("failed to resolve ancestor: %w", ancErr)
			}
		}
	}

	// The resolved path must be inside the resolved root. We require a path
	// separator between the root and any suffix so /workspace-other doesn't
	// satisfy a prefix match for /workspace.
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return "", ErrPathEscapesRoot
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrPathEscapesRoot
	}

	return resolved, nil
}

// WorkspaceRootFromInput extracts a validated workspace root from tool input.
// Returns ("", nil) if the key is absent, giving callers a chance to apply
// policy (refuse vs fall back) based on their own rules.
func WorkspaceRootFromInput(input map[string]interface{}) (string, error) {
	raw, ok := input["workspace_root"].(string)
	if !ok || raw == "" {
		return "", nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("invalid workspace_root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace_root does not exist: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace_root must be a directory")
	}
	return abs, nil
}

// shellMetacharacters are refused in command.exec arguments even after shell
// parsing, to prevent agents from chaining commands or redirecting I/O by
// embedding them in a single token. These are detected in the parsed tokens
// because shellquote will already have stripped syntactic quoting.
var shellMetacharacters = []string{
	";", "&&", "||", "|", ">", ">>", "<", "<<", "`", "$(",
}

// ParseCommand tokenizes a command string the way a POSIX shell would, then
// refuses any token that contains shell metacharacters. The goal is to give
// agents a way to run `git status` without giving them `rm -rf /; curl evil`.
func ParseCommand(command string) ([]string, error) {
	tokens, err := shellquote.Split(command)
	if err != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeCommandUnsafe,
			Title:   "Invalid command",
			Message: "Nomi couldn't understand that command. Try writing it more simply, like 'git status'.",
		}
	}
	if len(tokens) == 0 {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeCommandUnsafe,
			Title:   "Empty command",
			Message: "The command is empty. Nomi needs a real command to run.",
		}
	}
	for _, tok := range tokens {
		for _, m := range shellMetacharacters {
			if strings.Contains(tok, m) {
				return nil, &domain.UserError{
					Code:    domain.ErrCodeCommandUnsafe,
					Title:   "Unsafe command",
					Message: fmt.Sprintf("Commands with %q aren't allowed for safety. Run multiple simple commands instead.", m),
				}
			}
		}
	}
	return tokens, nil
}

// allowlistedEnvKeys are the environment variables forwarded to subprocesses
// started by command.exec. Anything else in the daemon's env (including
// provider secrets, bot tokens, AWS credentials, etc.) is dropped.
var allowlistedEnvKeys = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TERM",
	"TMPDIR",
	"TZ",
	"SHELL",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
}

// BuildSandboxEnv returns a minimal environment derived from the daemon env
// plus any caller-supplied overrides. Overrides win over inherited values.
func BuildSandboxEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(allowlistedEnvKeys)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for k, v := range overrides {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
		seen[k] = true
	}
	for _, k := range allowlistedEnvKeys {
		if seen[k] {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return env
}
