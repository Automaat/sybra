//go:build !e2e

package sybra

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"go.uber.org/goleak"
)

// TestMain (unit build) enforces goroutine cleanliness for the App service
// layer. The e2e build provides its own TestMain in e2e_workflow_test.go,
// where leaked Wails/agent goroutines are expected during long-running
// scenarios.
//
// Known long-lived backgrounders that have no Stop hook today are ignored
// below. Trim this list as packages add explicit teardown.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra tests require git on PATH:", err)
		os.Exit(1)
	}
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("runtime.cgocall"),
		goleak.IgnoreAnyFunction("net/http.(*Transport).dialConnFor"),
		// fsnotify watcher goroutine; App.Close cancels its ctx but
		// the OS-level kqueue loop returns asynchronously.
		goleak.IgnoreAnyFunction("github.com/fsnotify/fsnotify.(*Watcher).readEvents"),
		// SSE broker fanout loop; lifetime tied to the http.Server
		// which unit tests don't shut down cleanly.
		goleak.IgnoreAnyFunction("github.com/Automaat/sybra/internal/sse.(*Broker).run"),
	)
}
