package github

import (
	"fmt"
	"slices"
	"testing"
)

func TestCloseIssueWith(t *testing.T) {
	t.Parallel()

	t.Run("with comment", func(t *testing.T) {
		t.Parallel()
		rec := &recordingExecer{}
		if err := closeIssueWith(rec, "o/r", 42, "done"); err != nil {
			t.Fatalf("closeIssueWith: %v", err)
		}
		want := []string{"issue", "close", "42", "--repo", "o/r", "--reason", "completed", "--comment", "done"}
		if !slices.Equal(rec.lastArgs, want) {
			t.Fatalf("args = %v, want %v", rec.lastArgs, want)
		}
	})

	t.Run("without comment omits flag", func(t *testing.T) {
		t.Parallel()
		rec := &recordingExecer{}
		if err := closeIssueWith(rec, "o/r", 7, ""); err != nil {
			t.Fatalf("closeIssueWith: %v", err)
		}
		if slices.Contains(rec.lastArgs, "--comment") {
			t.Fatalf("did not expect --comment: %v", rec.lastArgs)
		}
	})

	t.Run("propagates error", func(t *testing.T) {
		t.Parallel()
		rec := &recordingExecer{err: fmt.Errorf("exit 1"), output: []byte("not found")}
		if err := closeIssueWith(rec, "o/r", 9, ""); err == nil {
			t.Fatal("expected error")
		}
	})
}
