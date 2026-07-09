package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/task"
)

// contextFileName is the per-worktree identity beacon written into every
// task worktree. Plan and implementation agents read it (or are prompted to)
// to ground their work in the correct task — defending against LLM
// confabulation when sibling worktrees in the same project expose unrelated
// branches via shared bare-repo refs.
const contextFileName = ".sybra-context.md"

// EvidenceDirName is the per-worktree directory a headless Playwright MCP
// server (see internal/agent/mcp.go) writes screenshots/console logs to.
// Exported so internal/agent's MCP preflight and internal/sybra's evidence
// importer share one name instead of duplicating the literal.
const EvidenceDirName = ".sybra-evidence"

// EvidenceBrowsersDirName is the child directory used for Playwright browser
// downloads. Keeping it under EvidenceDirName makes it writable in the
// per-worktree process sandbox and keeps all visual-verification scratch data
// git-excluded together.
const EvidenceBrowsersDirName = "browsers"

// EvidenceNPMCacheDirName is the child directory used for npx/npm package
// cache writes when launching the Playwright MCP server.
const EvidenceNPMCacheDirName = "npm-cache"

// ExcludeEvidenceDir git-excludes EvidenceDirName from wtPath so `git add -A`
// never sweeps agent-captured screenshots/logs into a commit. Delegates to the
// package-private addToInfoExclude — exported narrowly for internal/agent's
// MCP preflight, which runs before the normal run-agent worktree machinery
// touches info/exclude.
func ExcludeEvidenceDir(ctx context.Context, wtPath string) error {
	return addToInfoExclude(ctx, wtPath, EvidenceDirName)
}

// writeContextFile drops a markdown identity beacon at the worktree root and
// best-effort-excludes it from `git status` so agents don't accidentally
// commit it. Errors are surfaced to the caller; the beacon is load-bearing
// for the planner identity-pinning defense and a silent failure would let
// the same cross-task contamination class recur.
func writeContextFile(ctx context.Context, t task.Task, wtPath, branch string) error {
	body := renderContextFile(t, wtPath, branch)
	path := filepath.Join(wtPath, contextFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", contextFileName, err)
	}
	// Best-effort for the beacon: it carries only task identity (no work content),
	// so a failed exclude is a cosmetic git-status nuisance, not a leak.
	_ = addToInfoExclude(ctx, wtPath, contextFileName)
	return nil
}

func renderContextFile(t task.Task, wtPath, branch string) string {
	var sb strings.Builder
	sb.WriteString("# Sybra Task Context\n\n")
	sb.WriteString("This file is auto-written by Sybra into every task worktree as the\n")
	sb.WriteString("authoritative identity beacon for agents. **You are working on the task\n")
	sb.WriteString("described below — ignore any sibling branches you may observe via\n")
	sb.WriteString("`git branch -a`. They belong to other concurrent tasks against the same\n")
	sb.WriteString("project and have nothing to do with you.**\n\n")
	fmt.Fprintf(&sb, "- Task ID: `%s`\n", t.ID)
	if t.Title != "" {
		fmt.Fprintf(&sb, "- Title: %s\n", t.Title)
	}
	fmt.Fprintf(&sb, "- Branch: `%s`\n", branch)
	fmt.Fprintf(&sb, "- Worktree: `%s`\n", wtPath)
	if t.ProjectID != "" {
		fmt.Fprintf(&sb, "- Project: `%s`\n", t.ProjectID)
	}
	sb.WriteString("\nDo NOT commit this file. It is excluded via `.git/info/exclude`.\n")
	return sb.String()
}

// addToInfoExclude appends entry to the worktree's per-worktree info/exclude
// so `git status` and `git add -A` ignore it. Returns an error when exclusion
// could not be applied; callers for whom a committed file is merely cosmetic
// (the identity beacon) may ignore it, while callers seeding agent-authored
// content (NOTES.md) must fail closed — an unexcluded file would be swept into
// SanitizeWorktree's `git add -A` auto-commit and pushed to the PR. The
// "already present" case returns nil.
func addToInfoExclude(ctx context.Context, wtPath, entry string) error {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("resolve info/exclude: %w", err)
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wtPath, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir info dir: %w", err)
	}
	// A not-exist read is fine — the append below creates the file. Any other
	// read error (e.g. unreadable exclude) must surface: silently treating it as
	// empty would skip the dedup check and, worse, let a fail-closed caller
	// (ensureNotesFile) believe exclusion succeeded when it could not be verified.
	existing, rerr := os.ReadFile(excludePath)
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("read info/exclude: %w", rerr)
	}
	line := "/" + entry
	for raw := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(raw) == line {
			return nil
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open info/exclude: %w", err)
	}
	defer func() { _ = f.Close() }()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		return fmt.Errorf("append info/exclude: %w", err)
	}
	return nil
}

func excludeWorkflowScratchFiles(ctx context.Context, wtPath string) error {
	var errs []error
	for _, entry := range []string{".sybra-review-*.md", ".sybra-diff-*.patch", ".sybra-plan-*.md", ".sybra-plan-*.json", ".sybra-critique-*.md"} {
		if err := addToInfoExclude(ctx, wtPath, entry); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry, err))
		}
	}
	return errors.Join(errs...)
}
