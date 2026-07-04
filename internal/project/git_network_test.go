package project

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsTransientNetworkError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ssh connection refused", errors.New("ssh: connect to host github.com port 22: Connection refused"), true},
		{"dns failure", errors.New("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com"), true},
		{"connection reset", errors.New("fatal: Connection reset by peer"), true},
		{"timed out", errors.New("ssh: connect to host github.com port 22: Operation timed out"), true},
		{"content conflict", errors.New("CONFLICT (content): Merge conflict in foo.go"), false},
		{"auth failure", errors.New("fatal: Authentication failed for 'https://github.com/x/y.git/'"), false},
		{"missing repo", errors.New("fatal: repository 'https://github.com/x/y.git/' not found"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransientNetworkError(tc.err); got != tc.want {
				t.Errorf("IsTransientNetworkError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithNetworkRetry_RetriesOnTransientNetworkError(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0, 0}
	gitOpRetrySleep = func(time.Duration) {}

	var attempts int32
	err := withNetworkRetry(func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return fmt.Errorf("ssh: connect to host github.com port 22: Connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withNetworkRetry: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestWithNetworkRetry_DoesNotRetryNonNetworkErrors(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0, 0}
	gitOpRetrySleep = func(time.Duration) {
		t.Fatal("should not sleep/retry a non-network error")
	}

	wantErr := errors.New("CONFLICT (content): Merge conflict in foo.go")
	var attempts int32
	err := withNetworkRetry(func() error {
		atomic.AddInt32(&attempts, 1)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for non-network errors)", attempts)
	}
}

func TestWithNetworkRetry_GivesUpAfterExhaustingBackoffs(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0}
	gitOpRetrySleep = func(time.Duration) {}

	var attempts int32
	netErr := errors.New("fatal: Network is unreachable")
	err := withNetworkRetry(func() error {
		atomic.AddInt32(&attempts, 1)
		return netErr
	})
	if !errors.Is(err, netErr) {
		t.Fatalf("err = %v, want %v", err, netErr)
	}
	if want := int32(1 + len(gitOpRetryBackoffs)); attempts != want {
		t.Errorf("attempts = %d, want %d", attempts, want)
	}
}
