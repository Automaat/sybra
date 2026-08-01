package health

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestCheckQuarantinedTasks(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("no entries", func(t *testing.T) {
		t.Parallel()
		if got := checkQuarantinedTasks(nil, now); len(got) != 0 {
			t.Errorf("checkQuarantinedTasks(nil) = %+v, want empty", got)
		}
	})

	t.Run("one finding per entry", func(t *testing.T) {
		t.Parallel()
		entries := []task.QuarantineEntry{
			{File: "abc123.md", Reason: "invalid frontmatter: expected --- delimiters", QuarantinedAt: now},
		}
		findings := checkQuarantinedTasks(entries, now)
		if len(findings) != 1 {
			t.Fatalf("len(findings) = %d, want 1", len(findings))
		}
		f := findings[0]
		if f.Category != CatTaskQuarantine {
			t.Errorf("Category = %q, want %q", f.Category, CatTaskQuarantine)
		}
		if f.Severity != SeverityCritical {
			t.Errorf("Severity = %q, want critical", f.Severity)
		}
		if f.TaskID != "abc123.md" {
			t.Errorf("TaskID = %q, want %q", f.TaskID, "abc123.md")
		}
		if f.Evidence["reason"] != entries[0].Reason {
			t.Errorf("Evidence[reason] = %v, want %q", f.Evidence["reason"], entries[0].Reason)
		}
	})
}

func TestCheckQuarantinedTasksFingerprintDedupsPerFile(t *testing.T) {
	t.Parallel()
	now := time.Now()
	entry := task.QuarantineEntry{File: "abc123.md", Reason: "boom", QuarantinedAt: now}

	first := checkQuarantinedTasks([]task.QuarantineEntry{entry}, now)
	entry.Reason = "boom (still failing)"
	second := checkQuarantinedTasks([]task.QuarantineEntry{entry}, now.Add(time.Minute))

	first[0].Fingerprint = FingerprintFor(&first[0])
	second[0].Fingerprint = FingerprintFor(&second[0])
	if first[0].Fingerprint != second[0].Fingerprint {
		t.Errorf("fingerprint changed across ticks for the same file: %q vs %q", first[0].Fingerprint, second[0].Fingerprint)
	}
}
