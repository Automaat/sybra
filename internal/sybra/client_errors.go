package sybra

import (
	"errors"
	"net/http"

	"github.com/Automaat/sybra/internal/fsutil"
)

// clientError is a user-facing error that the HTTP API handler surfaces
// directly (status + message) instead of sanitizing to 500 internal error.
// Implements httpapi.ClientError without importing that package.
type clientError struct {
	status int
	msg    string
}

func (e *clientError) Error() string   { return e.msg }
func (e *clientError) HTTPStatus() int { return e.status }

func validationError(msg string) error {
	return &clientError{status: http.StatusBadRequest, msg: msg}
}

func conflictError(msg string) error {
	return &clientError{status: http.StatusConflict, msg: msg}
}

func unavailableError(msg string) error {
	return &clientError{status: http.StatusServiceUnavailable, msg: msg}
}

// mapLockTimeout translates an fsutil lock-acquisition timeout into a retryable
// 503 clientError so it survives HTTP flattening (otherwise the handler's
// stripErrorResult sanitizes it to a generic 500) and is detectable as
// retryable at the CLI layer. Any other error (including nil) is returned
// unchanged. The error's message carries the lock-path/holder-pid diagnostic.
func mapLockTimeout(err error) error {
	if err != nil && errors.Is(err, fsutil.ErrLockTimeout) {
		return unavailableError(err.Error())
	}
	return err
}
