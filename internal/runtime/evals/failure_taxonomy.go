package evals

import (
	"errors"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// FailureClass is a stable bucket used for reliability reporting and
// regression tracking across planner/tool/runtime failures.
type FailureClass string

const (
	FailureUnknown              FailureClass = "unknown"
	FailurePlanner              FailureClass = "planner_failed"
	FailureToolNotFound         FailureClass = "tool_not_found"
	FailureToolExecution        FailureClass = "tool_execution_failed"
	FailurePermissionDenied     FailureClass = "permission_denied"
	FailureApprovalDenied       FailureClass = "approval_denied"
	FailureRateLimited          FailureClass = "rate_limited"
	FailureContextTooLong       FailureClass = "llm_context_too_long"
	FailureNoProviderConfigured FailureClass = "no_llm_provider"
	FailureValidation           FailureClass = "invalid_request"
)

// ClassifyError maps runtime/API/user errors to a stable taxonomy.
// This is intentionally conservative: unknowns are explicit so dashboards
// can track classification coverage over time.
func ClassifyError(err error) FailureClass {
	if err == nil {
		return FailureUnknown
	}

	var ue *domain.UserError
	if errors.As(err, &ue) {
		switch ue.Code {
		case domain.ErrCodePlannerFailed:
			return FailurePlanner
		case domain.ErrCodeToolNotFound:
			return FailureToolNotFound
		case domain.ErrCodeToolExecution:
			return FailureToolExecution
		case domain.ErrCodePolicyDeny:
			return FailurePermissionDenied
		case domain.ErrCodeApprovalDenied, domain.ErrCodeApprovalRemembered:
			return FailureApprovalDenied
		case domain.ErrCodeLLMRateLimited, domain.ErrCodeRateLimited:
			return FailureRateLimited
		case domain.ErrCodeLLMContextTooLong:
			return FailureContextTooLong
		case domain.ErrCodeNoLLMProvider:
			return FailureNoProviderConfigured
		case domain.ErrCodePathOutsideRoot, domain.ErrCodeBinaryNotAllowed, domain.ErrCodeCommandUnsafe:
			return FailurePermissionDenied
		}
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "planner"):
		return FailurePlanner
	case strings.Contains(msg, "tool not found"):
		return FailureToolNotFound
	case strings.Contains(msg, "permission denied"):
		return FailurePermissionDenied
	case strings.Contains(msg, "rate limit"):
		return FailureRateLimited
	case strings.Contains(msg, "invalid"):
		return FailureValidation
	default:
		return FailureUnknown
	}
}
