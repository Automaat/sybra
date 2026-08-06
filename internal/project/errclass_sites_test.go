package project

import (
	"errors"
	"testing"
)

// TestIsTransientNetworkError_PinsLiterals pins the transport phrases this
// predicate matched before it moved onto errclass.
//
// It gates branch-conflict recovery, and worktree treats its true answer as
// ErrTransientFetch, which callers must never escalate to human-required. A
// phrase that is permanent but reads as transient here therefore retries
// forever with no human ever told.
func TestIsTransientNetworkError_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{msg: "connect: connection refused", want: true},
		{msg: "connection reset by peer", want: true},
		{msg: "connection timed out", want: true},
		{msg: "failed to connect to github.com", want: true},
		{msg: "couldn't connect to server", want: true},
		{msg: "fatal: could not resolve host: github.com", want: true},
		{msg: "couldn't resolve host name", want: true},
		{msg: "network is unreachable", want: true},
		{msg: "operation timed out", want: true},
		{msg: "temporary failure in name resolution", want: true},
		{msg: "no route to host", want: true},
		{msg: "ssh: connect to host github.com port 22: Connection timed out", want: true},
		{msg: "recv failure: Connection reset by peer", want: true},
		{msg: "net/http: TLS handshake timeout", want: true},
		{msg: "empty reply from server", want: true},
		{msg: "early EOF", want: true},
		{msg: "fatal: unexpected disconnect while reading sideband packet", want: true},

		// An x509 failure never self-heals, so a true answer here would loop without ever reaching a human.
		{msg: `tls: failed to verify certificate: x509: certificate signed by unknown authority`, want: false},
		{msg: "CONFLICT (content): Merge conflict in main.go", want: false},
		{msg: "Authentication failed for 'https://github.com/o/r.git/'", want: false},
		{msg: "", want: false},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			if got := IsTransientNetworkError(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("IsTransientNetworkError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
	if IsTransientNetworkError(nil) {
		t.Fatal("IsTransientNetworkError(nil) = true")
	}
}

// TestIsBadRefError_PinsLiterals pins the twelve needles that lived
// byte-identically here and in internal/workflow.
func TestIsBadRefError_PinsLiterals(t *testing.T) {
	for _, msg := range []string{
		"bad object HEAD",
		"fatal: bad object abc123",
		"not a valid object name",
		"invalid object",
		"fatal: Invalid revision range origin/main...HEAD",
		"missing object",
		"unable to read sha1 file",
		"error: object file .git/objects/ab/cdef is empty",
		"loose object",
		"unknown revision",
		"fatal: ambiguous argument 'origin/main'",
		"reference broken",
	} {
		if !IsBadRefError(errors.New(msg)) {
			t.Errorf("IsBadRefError(%q) = false, want true", msg)
		}
	}
	for _, msg := range []string{"connection refused", "Bad credentials", ""} {
		if IsBadRefError(errors.New(msg)) {
			t.Errorf("IsBadRefError(%q) = true, want false", msg)
		}
	}
}
