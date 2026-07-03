package worktree

import (
	"context"
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
func ensureNotesFile(ctx context.Context, wtPath string) error {
	if err := addToInfoExclude(ctx, wtPath, notes.FileName); err != nil {
		return fmt.Errorf("exclude %s (refusing to seed an unignored scratchpad): %w", notes.FileName, err)
	}
	path := filepath.Join(wtPath, notes.FileName)
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return nil // already present and now excluded — preserve contents
	case errors.Is(statErr, os.ErrNotExist):
		// 0600, not 0644: the scratchpad holds agent-authored (for work tasks,
		// work-derived) content, so keep it private to the user on multi-user hosts.
		if err := os.WriteFile(path, []byte(notes.SeedTemplate), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", notes.FileName, err)
		}
		return nil
	default:
		return fmt.Errorf("stat %s: %w", notes.FileName, statErr)
	}
}

// appendSetupFailureNote records a non-gating setup failure (see
// runSetupNonGating) into the worktree's NOTES.md scratchpad so a fix-role
// agent sees it as its starting signal — NOTES.md is already inlined into
// those agents' prompts via SeedWorkingMemory. Creates and git-excludes the
// scratchpad first since fix-role prepares don't otherwise seed it.
func appendSetupFailureNote(ctx context.Context, wtPath string, setupErr error) error {
	if err := ensureNotesFile(ctx, wtPath); err != nil {
		return err
	}
	path := filepath.Join(wtPath, notes.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", notes.FileName, err)
	}
	section := fmt.Sprintf(
		"\n## Setup failure (pre-existing)\n\n"+
			"Worktree setup failed before this agent started:\n\n```\n%s\n```\n\n"+
			"This is very likely the exact defect this task exists to fix — start there.\n",
		setupErr,
	)
	if err := os.WriteFile(path, append(data, []byte(section)...), 0o600); err != nil {
		return fmt.Errorf("append %s: %w", notes.FileName, err)
	}
	return nil
}
