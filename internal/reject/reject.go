// Package reject marks a refusal of the caller's request — a malformed update,
// a restore onto a live id, an unparseable repo URL — as distinct from a
// storage or network failure.
//
// It changes no message text. It exists so a server can tell the two apart:
// the HTTP API has to sanitize an operational failure, because those wrap an
// *fs.PathError or format an absolute path into the message, but a refusal is
// the caller's own mistake and has to reach them verbatim.
package reject

import (
	"errors"
	"fmt"
)

// Error is a refusal of the caller's request. Conflict distinguishes a
// request that is well-formed but collides with the current state, which a
// server answers 409, from malformed input, which it answers 400.
type Error struct {
	err      error
	Conflict bool
}

func (e *Error) Error() string { return e.err.Error() }

func (e *Error) Unwrap() error { return e.err }

// New builds a refusal with the given message.
func New(format string, a ...any) error {
	return &Error{err: fmt.Errorf(format, a...)}
}

// Conflicting builds a refusal of a well-formed request that collides with
// the current state.
func Conflicting(format string, a ...any) error {
	return &Error{err: fmt.Errorf(format, a...), Conflict: true}
}

// IsConflict reports whether err is, or wraps, a colliding refusal.
func IsConflict(err error) bool {
	var rejection *Error
	return errors.As(err, &rejection) && rejection.Conflict
}

// Is reports whether err is, or wraps, a refusal.
func Is(err error) bool {
	var rejection *Error
	return errors.As(err, &rejection)
}
