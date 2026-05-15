package sybra

import "net/http"

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
