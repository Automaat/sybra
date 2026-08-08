package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/gitexec"
)

func TestWorktreeOperationDetectsMergeMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	if err := gitexec.Run(ctx, gitexec.Options{Dir: dir}, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if got := WorktreeOperation(ctx, dir); got != "" {
		t.Fatalf("clean operation = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}
	if got := WorktreeOperation(ctx, dir); got != "merge" {
		t.Fatalf("operation = %q, want merge", got)
	}
}
