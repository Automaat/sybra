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

func unavailableError(msg string) error {
	return &clientError{status: http.StatusServiceUnavailable, msg: msg}
}

// notFoundError names the record that was missing. The stores' own miss
// errors embed the absolute path they looked at, which must not reach a
// client, so the identifier stands in for it.
func notFoundError(kind, id string) error {
	return &clientError{status: http.StatusNotFound, msg: kind + " " + id + " not found"}
}
