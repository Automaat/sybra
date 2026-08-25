package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrContended marks an error a database statement produced by waiting rather
// than by failing. Stores wrap their own statement errors with it; callers must
// never synthesize one from an error that did not come from this layer.
var ErrContended = errors.New("backend contended")

// Contended wraps err with ErrContended when it is the backend asking to be
// called again: a statement that spent its own deadline queueing, sqlite's
// SQLITE_BUSY, or postgres' serialization-failure and deadlock classes.
//
// Only a store may call this, and only on an error a database statement
// returned. A bare context.DeadlineExceeded is not classified anywhere else,
// because every http.Client timeout produces one too and a remote that is down
// must not read as a busy database.
func Contended(err error) error {
	if err == nil || errors.Is(err, ErrContended) {
		return err
	}
	if !isContentionSignal(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContended, err)
}

// IsContention reports whether err was marked by Contended.
func IsContention(err error) bool {
	return err != nil && errors.Is(err, ErrContended)
}

func isContentionSignal(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	for _, signal := range contentionSignals {
		if strings.Contains(msg, signal) {
			return true
		}
	}
	return false
}

var contentionSignals = []string{
	"SQLITE_BUSY",
	"database is locked",
	"SQLSTATE 40001",
	"SQLSTATE 40P01",
	"SQLSTATE 55P03",
	"SQLSTATE 57014",
}
