package health

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func degradedTask(id, path, parseErr string) task.Task {
	return task.Task{ID: id, FilePath: path, Degraded: true, ParseError: parseErr}
}

func TestCheckUnreadableTasks(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("no tasks", func(t *testing.T) {
		t.Parallel()
		if got := checkUnreadableTasks(nil, now); len(got) != 0 {
			t.Errorf("checkUnreadableTasks(nil) = %+v, want empty", got)
		}
	})

	t.Run("ignores readable tasks", func(t *testing.T) {
		t.Parallel()
		tasks := []task.Task{{ID: "abc123", FilePath: "/tasks/abc123.md"}}
		if got := checkUnreadableTasks(tasks, now); len(got) != 0 {
			t.Errorf("checkUnreadableTasks(readable) = %+v, want empty", got)
		}
	})

	t.Run("one finding per degraded task", func(t *testing.T) {
		t.Parallel()
		tasks := []task.Task{
			{ID: "abc123", FilePath: "/tasks/abc123.md"},
			degradedTask("unreadable-ff00", "/tasks/bad.md", "invalid frontmatter: expected --- delimiters"),
		}
		findings := checkUnreadableTasks(tasks, now)
		if len(findings) != 1 {
			t.Fatalf("len(findings) = %d, want 1", len(findings))
		}
		f := findings[0]
		if f.Category != CatTaskUnreadable {
			t.Errorf("Category = %q, want %q", f.Category, CatTaskUnreadable)
		}
		if f.Severity != SeverityCritical {
			t.Errorf("Severity = %q, want critical", f.Severity)
		}
		if f.TaskID != "unreadable-ff00" {
			t.Errorf("TaskID = %q, want %q", f.TaskID, "unreadable-ff00")
		}
		if f.Evidence["file"] != "bad.md" {
			t.Errorf("Evidence[file] = %v, want bad.md", f.Evidence["file"])
		}
		if f.Evidence["reason"] != tasks[1].ParseError {
			t.Errorf("Evidence[reason] = %v, want %q", f.Evidence["reason"], tasks[1].ParseError)
		}
	})
}

func TestCheckUnreadableTasksFingerprintDedupsPerFile(t *testing.T) {
	t.Parallel()
	now := time.Now()
	entry := degradedTask("unreadable-ff00", "/tasks/bad.md", "boom")

	first := checkUnreadableTasks([]task.Task{entry}, now)
	entry.ParseError = "boom (still failing)"
	second := checkUnreadableTasks([]task.Task{entry}, now.Add(time.Minute))

	first[0].Fingerprint = FingerprintFor(&first[0])
	second[0].Fingerprint = FingerprintFor(&second[0])
	if first[0].Fingerprint != second[0].Fingerprint {
		t.Errorf("fingerprint changed across ticks for the same file: %q vs %q", first[0].Fingerprint, second[0].Fingerprint)
	}
}
