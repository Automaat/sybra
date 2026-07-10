package agent

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

// conflictError reports a precondition failure — the agent is in a state
// (finalizing, no live stdin transport, queue full) that makes the request
// impossible to satisfy right now, not a caller input mistake.
func conflictError(msg string) error {
	return &ClientError{status: http.StatusConflict, msg: msg}
}
