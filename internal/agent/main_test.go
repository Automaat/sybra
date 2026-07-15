package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces goroutine cleanliness across agent tests. The agent
// package launches stream readers, tmux poll loops, and headless process
// supervisors — every test must wire them to a context that cancels by
// teardown. A failure here usually points at a missing <-ctx.Done() branch
// or an unbuffered channel that never drains.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("runtime.cgocall"),
		// The default http.Transport keeps idle connections in a background
		// goroutine; harmless and process-global.
		goleak.IgnoreAnyFunction("net/http.(*Transport).dialConnFor"),
	)
}
