package worktree

import (
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

// writeContextFile drops a markdown identity beacon at the worktree root and
// best-effort-excludes it from `git status` so agents don't accidentally
// commit it. Errors are surfaced to the caller; the beacon is load-bearing
// for the planner identity-pinning defense and a silent failure would let
// the same cross-task contamination class recur.
func writeContextFile(t task.Task, wtPath, branch string) error {
	body := renderContextFile(t, wtPath, branch)
	path := filepath.Join(wtPath, contextFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", contextFileName, err)
	}
	addToInfoExclude(wtPath, contextFileName)
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
// so `git status` and `git add -A` ignore it. Best-effort: a missing or
// unwritable exclude file is logged elsewhere but not fatal — the beacon is
// still usable, the agent just risks staging it.
func addToInfoExclude(wtPath, entry string) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wtPath, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return
	}
	existing, _ := os.ReadFile(excludePath)
	line := "/" + entry
	for raw := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(raw) == line {
			return
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	_, _ = f.WriteString(prefix + line + "\n")
}
