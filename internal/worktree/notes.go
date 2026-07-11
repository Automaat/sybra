package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/notes"
)

const setupFailureMarkerName = ".sybra-setup-failure"

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

// AppendNote appends a markdown section to the worktree scratchpad after
// ensuring NOTES.md exists and stays git-excluded. When marker is non-empty,
// an existing identical marker makes the append a no-op so callers can safely
// retry without duplicating a section.
func AppendNote(ctx context.Context, wtPath, marker, section string) error {
	if err := ensureNotesFile(ctx, wtPath); err != nil {
		return err
	}
	path := filepath.Join(wtPath, notes.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", notes.FileName, err)
	}
	if marker != "" && strings.Contains(string(data), marker) {
		return nil
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+section+"\n")...), 0o600); err != nil {
		return fmt.Errorf("append %s: %w", notes.FileName, err)
	}
	return nil
}

// appendSetupFailureNote records a non-gating setup failure (see
// runSetupNonGating) into the worktree's NOTES.md scratchpad so a fix-role
// agent sees it as its starting signal — NOTES.md is already inlined into
// those agents' prompts via SeedWorkingMemory. Creates and git-excludes the
// scratchpad first since fix-role prepares don't otherwise seed it.
func appendSetupFailureNote(ctx context.Context, wtPath string, setupErr error) error {
	section := fmt.Sprintf(
		"## Setup failure (pre-existing)\n\n"+
			"Worktree setup failed before this agent started:\n\n```\n%s\n```\n\n"+
			"This is very likely the exact defect this task exists to fix — start there.\n",
		setupErr,
	)
	return AppendNote(ctx, wtPath, "", section)
}

func writeSetupFailureMarker(ctx context.Context, wtPath string, setupErr error) error {
	if err := addToInfoExclude(ctx, wtPath, setupFailureMarkerName); err != nil {
		return fmt.Errorf("exclude %s: %w", setupFailureMarkerName, err)
	}
	path := filepath.Join(wtPath, setupFailureMarkerName)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(setupErr.Error())+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", setupFailureMarkerName, err)
	}
	return nil
}

func clearSetupFailureMarker(wtPath string) error {
	path := filepath.Join(wtPath, setupFailureMarkerName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", setupFailureMarkerName, err)
	}
	return nil
}

// ReadSetupFailureMarker reports whether a fix-role worktree carried a
// non-fatal setup failure during prepare, without requiring callers to parse
// NOTES.md. The returned text is advisory context for logs/circuit breakers.
func ReadSetupFailureMarker(wtPath string) (message string, exists bool, err error) {
	data, err := os.ReadFile(filepath.Join(wtPath, setupFailureMarkerName))
	switch {
	case err == nil:
		return strings.TrimSpace(string(data)), true, nil
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("read %s: %w", setupFailureMarkerName, err)
	}
}
