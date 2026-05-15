package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorCode identifies the failure class in a JSON error response.
type ErrorCode string

const (
	ErrCodeNotFound   ErrorCode = "not_found"
	ErrCodeValidation ErrorCode = "validation_error"
	ErrCodeConflict   ErrorCode = "conflict"
	ErrCodeTooLarge   ErrorCode = "payload_too_large"
	ErrCodeInternal   ErrorCode = "internal_error"
)

// ClientError is implemented by service errors that are safe to surface to
// HTTP clients (validation failures, precondition/state errors). The handler
// passes the message and status through instead of sanitizing to 500.
// Service packages implement this locally — no import of httpapi required.
type ClientError interface {
	error
	HTTPStatus() int
}

type errorEnvelope struct {
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
}

func respondError(w http.ResponseWriter, logger *slog.Logger, status int, code ErrorCode, clientMsg string) {
	if status >= 500 {
		logger.Error("httpapi.error", "status", status, "code", code)
	} else {
		logger.Warn("httpapi.error", "status", status, "code", code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{Error: clientMsg, Code: code}); err != nil {
		logger.Error("httpapi.error.encode", "status", status, "code", code, "err", err)
	}
}
