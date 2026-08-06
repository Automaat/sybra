package agent

import (
	"errors"
	"fmt"
	"testing"
)

// handleError re-classifies every fatal error from scratch, so the kind stamped
// at the delivery site only survives if classifyAgentError recognises it too.
// Without that, an undelivered prompt lands in the generic "crash" bucket and
// completion counts it as a real verdict.
func TestClassifyAgentError_PromptUndelivered(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("deliver initial prompt: %w: %w", errPromptUndelivered,
		errors.New("write stdin: timed out after 2m0s, pipe closed"))

	if got := classifyAgentError(wrapped); got != ErrorKindPromptUndelivered {
		t.Fatalf("classifyAgentError(delivery failure) = %q, want %q", got, ErrorKindPromptUndelivered)
	}
}

// The wrapped error text contains "i/o timeout"-adjacent wording that the
// substring table would otherwise classify as a git/network failure.
func TestClassifyAgentError_PromptUndeliveredBeatsSubstringTable(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("deliver initial prompt: %w: %w", errPromptUndelivered,
		errors.New("write stdin: i/o timeout"))

	if got := classifyAgentError(wrapped); got != ErrorKindPromptUndelivered {
		t.Fatalf("classifyAgentError(i/o timeout delivery failure) = %q, want %q", got, ErrorKindPromptUndelivered)
	}
}

func TestHandleError_PreservesPromptUndeliveredKind(t *testing.T) {
	t.Parallel()

	m, _ := newTestManager(t)
	a := &Agent{ID: "a1"}
	wrapped := fmt.Errorf("deliver initial prompt: %w: %w", errPromptUndelivered,
		errors.New("write stdin: timed out after 2m0s, pipe closed"))

	m.handleError(t.Context(), a, wrapped)

	if got := a.GetErrorKind(); got != ErrorKindPromptUndelivered {
		t.Fatalf("GetErrorKind() after handleError = %q, want %q", got, ErrorKindPromptUndelivered)
	}
}
