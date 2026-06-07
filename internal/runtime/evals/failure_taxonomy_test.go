package evals

import (
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestClassifyError_UserErrorCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"planner", &domain.UserError{Code: domain.ErrCodePlannerFailed, Message: "planner failed"}, FailurePlanner},
		{"tool-not-found", &domain.UserError{Code: domain.ErrCodeToolNotFound, Message: "missing tool"}, FailureToolNotFound},
		{"tool-exec", &domain.UserError{Code: domain.ErrCodeToolExecution, Message: "exec failed"}, FailureToolExecution},
		{"policy-deny", &domain.UserError{Code: domain.ErrCodePolicyDeny, Message: "denied"}, FailurePermissionDenied},
		{"approval-deny", &domain.UserError{Code: domain.ErrCodeApprovalDenied, Message: "approval denied"}, FailureApprovalDenied},
		{"rate-limit", &domain.UserError{Code: domain.ErrCodeRateLimited, Message: "rate limited"}, FailureRateLimited},
		{"context-too-long", &domain.UserError{Code: domain.ErrCodeLLMContextTooLong, Message: "too long"}, FailureContextTooLong},
		{"no-provider", &domain.UserError{Code: domain.ErrCodeNoLLMProvider, Message: "no provider"}, FailureNoProviderConfigured},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyError(tt.err); got != tt.want {
				t.Fatalf("ClassifyError()=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestClassifyError_FallbackByMessage(t *testing.T) {
	tests := []struct {
		msg  string
		want FailureClass
	}{
		{"planner output malformed", FailurePlanner},
		{"tool not found: x", FailureToolNotFound},
		{"permission denied", FailurePermissionDenied},
		{"rate limit exceeded", FailureRateLimited},
		{"invalid payload", FailureValidation},
		{"something else", FailureUnknown},
	}

	for _, tt := range tests {
		err := errors.New(tt.msg)
		if got := ClassifyError(err); got != tt.want {
			t.Fatalf("msg=%q got=%s want=%s", tt.msg, got, tt.want)
		}
	}
}
