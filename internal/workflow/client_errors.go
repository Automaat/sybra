package workflow

import "net/http"

// ClientError is a user-facing error that internal/httpapi's handler
// surfaces directly (status + message) instead of sanitizing it to a
// generic 500 internal error. Implements httpapi.ClientError structurally
// (error + HTTPStatus() int) without importing that package.
type ClientError struct {
	status int
	msg    string
}

func (e *ClientError) Error() string   { return e.msg }
func (e *ClientError) HTTPStatus() int { return e.status }

// conflictError reports that a task is not in the workflow state a human
// action (approve/reject/input) requires right now — retrying the same call
// won't help until the task's state changes, but it's not a malformed
// request.
func conflictError(msg string) error {
	return &ClientError{status: http.StatusConflict, msg: msg}
}

// validationError reports a caller input mistake, e.g. an action name the
// current wait_human step does not accept.
func validationError(msg string) error {
	return &ClientError{status: http.StatusBadRequest, msg: msg}
}
