package domain

// UserError is a user-facing error with a machine-readable code and a
// human-readable message. When an API handler returns a UserError, the
// transport layer can include the code in the JSON response so the UI
// can show a tailored message or action button.
type UserError struct {
	Code    string `json:"code"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
	Action  string `json:"action,omitempty"` // e.g. "Open Assistant Builder"
}

func (e *UserError) Error() string { return e.Message }

// Common user-facing error codes. Keep these stable — the frontend keys
// off them for localized copy and action buttons.
const (
	ErrCodeCeilingViolation   = "ceiling_violation"
	ErrCodePolicyDeny         = "policy_deny"
	ErrCodeApprovalDenied     = "approval_denied"
	ErrCodeApprovalRemembered = "approval_remembered"
	ErrCodeRateLimited        = "rate_limited"
	ErrCodeRetryExhausted     = "retry_exhausted"
	ErrCodeToolNotFound       = "tool_not_found"
	ErrCodeToolExecution      = "tool_execution_failed"
	ErrCodePlannerFailed      = "planner_failed"
	ErrCodeNoLLMProvider      = "no_llm_provider"
	ErrCodeLLMInvalidKey      = "llm_invalid_key"
	ErrCodeLLMRateLimited     = "llm_rate_limited"
	ErrCodeLLMContextTooLong  = "llm_context_too_long"
	ErrCodeMissingWorkspace   = "missing_workspace"
	ErrCodePathOutsideRoot    = "path_outside_root"
	ErrCodeBinaryNotAllowed   = "binary_not_allowed"
	ErrCodeCommandTimeout     = "command_timeout"
	ErrCodeCommandUnsafe      = "command_unsafe"
	// Patch-tool specific. Distinct codes so the planner / replan loop
	// can react to "file doesn't exist" (read+retry) vs "diff too big"
	// (split into smaller patches) vs generic apply failure.
	ErrCodePatchFileMissing = "patch_file_missing"
	ErrCodePatchTooLarge    = "patch_too_large"
	ErrCodePatchApplyFailed = "patch_apply_failed"
)
