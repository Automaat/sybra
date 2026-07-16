package health

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/sandbox"
)

func TestCheckSandboxCleanupFailures(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("no entries", func(t *testing.T) {
		t.Parallel()
		if got := checkSandboxCleanupFailures(nil, now); len(got) != 0 {
			t.Errorf("checkSandboxCleanupFailures(nil) = %+v, want empty", got)
		}
	})

	t.Run("one finding per entry with bytes retained", func(t *testing.T) {
		t.Parallel()
		entries := []sandbox.QuarantineEntry{
			{TaskID: "task-a", Path: "/data/sandboxes/task-a", BytesRetained: 4096, Attempts: 3, LastError: "permission denied"},
			{TaskID: "task-b", Path: "/data/sandboxes/task-b", BytesRetained: 8192, Attempts: 1, LastError: "device or resource busy"},
		}
		findings := checkSandboxCleanupFailures(entries, now)
		if len(findings) != 2 {
			t.Fatalf("len(findings) = %d, want 2", len(findings))
		}
		byTask := map[string]Finding{}
		for _, f := range findings {
			byTask[f.TaskID] = f
		}
		a, ok := byTask["task-a"]
		if !ok {
			t.Fatal("missing finding for task-a")
		}
		if a.Category != CatSandboxCleanup {
			t.Errorf("Category = %q, want %q", a.Category, CatSandboxCleanup)
		}
		if a.Severity != SeverityCritical {
			t.Errorf("Severity = %q, want critical", a.Severity)
		}
		if a.Evidence["bytes_retained"] != int64(4096) {
			t.Errorf("Evidence[bytes_retained] = %v, want 4096", a.Evidence["bytes_retained"])
		}
		if a.Evidence["attempts"] != 3 {
			t.Errorf("Evidence[attempts] = %v, want 3", a.Evidence["attempts"])
		}
	})
}

func TestCheckSandboxCleanupFailuresFingerprintDedupsPerTask(t *testing.T) {
	t.Parallel()
	now := time.Now()
	entry := sandbox.QuarantineEntry{TaskID: "task-a", BytesRetained: 100, Attempts: 1, LastError: "boom"}

	first := checkSandboxCleanupFailures([]sandbox.QuarantineEntry{entry}, now)
	entry.Attempts = 2
	entry.BytesRetained = 150
	second := checkSandboxCleanupFailures([]sandbox.QuarantineEntry{entry}, now.Add(time.Minute))

	first[0].Fingerprint = FingerprintFor(&first[0])
	second[0].Fingerprint = FingerprintFor(&second[0])
	if first[0].Fingerprint != second[0].Fingerprint {
		t.Errorf("fingerprint changed across ticks for the same task: %q vs %q", first[0].Fingerprint, second[0].Fingerprint)
	}
}
