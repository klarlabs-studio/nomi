package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
)

// respondError writes a JSON error response. If err is a *domain.UserError,
// the response includes structured code/title/message/action fields so the UI
// can render a tailored message and action button.
func respondError(c *gin.Context, status int, err error) {
	if ue, ok := err.(*domain.UserError); ok {
		payload := gin.H{
			"error":   ue.Message,
			"code":    ue.Code,
			"title":   ue.Title,
			"message": ue.Message,
		}
		if ue.Action != "" {
			payload["action"] = ue.Action
		}
		c.JSON(status, payload)
		return
	}

	message := "Unexpected server error. Please retry."
	code := "internal_error"
	title := "Unexpected Error"

	if status >= 400 && status < 500 {
		message = "The request is invalid. Please review the input and try again."
		code = "invalid_request"
		title = "Invalid Request"
	}
	if status == http.StatusNotFound {
		message = "The requested resource was not found."
		code = "not_found"
		title = "Not Found"
	}
	if err != nil {
		message = err.Error()
	}

	c.JSON(status, gin.H{
		"error":   message,
		"code":    code,
		"title":   title,
		"message": message,
	})
}

func respondValidationError(c *gin.Context, message string) {
	respondError(c, http.StatusBadRequest, &domain.UserError{
		Code:    "invalid_request",
		Title:   "Invalid Request",
		Message: message,
	})
}

func respondNotFound(c *gin.Context, message string) {
	respondError(c, http.StatusNotFound, &domain.UserError{
		Code:    "not_found",
		Title:   "Not Found",
		Message: message,
	})
}

func respondInternal(c *gin.Context, message string, err error) {
	msg := message
	if msg == "" {
		msg = "Unexpected server error. Please retry."
	}
	if err != nil {
		msg = msg + ": " + err.Error()
	}
	respondError(c, http.StatusInternalServerError, &domain.UserError{
		Code:    "internal_error",
		Title:   "Unexpected Error",
		Message: msg,
	})
}
