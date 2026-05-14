package workflow

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test to catch goroutines left running
// past their controlling context. The workflow engine spawns background
// dispatch/advance goroutines; stragglers indicate missing context
// cancellation or unclosed channels.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// Wails v3 internals start a CGO finalizer goroutine on macOS that
		// never exits; safe to ignore in unit tests that don't touch Wails.
		goleak.IgnoreTopFunction("runtime.cgocall"),
	)
}
