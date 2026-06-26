package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/notes"
)

// ensureNotesFile seeds the agent working-memory scratchpad (notes.FileName) at
// the worktree root and git-excludes it, mirroring the identity beacon. Unlike
// the beacon it fails closed: exclusion is applied BEFORE the file is created,
// and a failed exclude aborts the seed. The scratchpad holds agent-authored
// (for work tasks, work-derived) content, so an unexcluded NOTES.md would be
// swept into SanitizeWorktree's `git add -A` auto-commit and pushed to the PR —
// a confidentiality leak. No scratchpad is safer than a committable one.
//
// The file is created only when absent: it is agent-maintained memory that must
// persist across worktree reuse/resume, so a re-prepare must never clobber
// accumulated notes. Exclusion is idempotent (dedup'd on the line) and shared
// across a clone's linked worktrees, so re-asserting it every call is cheap and
// safe.
func ensureNotesFile(wtPath string) error {
	if err := addToInfoExclude(wtPath, notes.FileName); err != nil {
		return fmt.Errorf("exclude %s (refusing to seed an unignored scratchpad): %w", notes.FileName, err)
	}
	path := filepath.Join(wtPath, notes.FileName)
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return nil // already present and now excluded — preserve contents
	case errors.Is(statErr, os.ErrNotExist):
		if err := os.WriteFile(path, []byte(notes.SeedTemplate), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", notes.FileName, err)
		}
		return nil
	default:
		return fmt.Errorf("stat %s: %w", notes.FileName, statErr)
	}
}
