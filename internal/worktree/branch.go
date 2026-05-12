package worktree

import (
	"strings"

	"github.com/Automaat/sybra/internal/task"
)

var conventionalBranchTypes = map[string]struct{}{
	"feat":     {},
	"fix":      {},
	"docs":     {},
	"style":    {},
	"refactor": {},
	"perf":     {},
	"test":     {},
	"build":    {},
	"ci":       {},
	"chore":    {},
	"revert":   {},
}

func branchNameForTask(t task.Task) string {
	if t.Branch != "" {
		return t.Branch
	}
	return branchPrefixForTask(t) + "/" + t.DirName()
}

func branchPrefixForTask(t task.Task) string {
	if typ, ok := conventionalType(t.Title); ok {
		return typ
	}
	switch t.TaskType {
	case task.TaskTypeDebug:
		return "fix"
	case task.TaskTypeResearch, task.TaskTypeChat:
		return "chore"
	default:
		return "chore"
	}
}

func conventionalType(title string) (string, bool) {
	idx := strings.IndexByte(title, ':')
	if idx <= 0 {
		return "", false
	}
	head := strings.TrimSpace(title[:idx])
	head = strings.TrimSuffix(head, "!")
	if open := strings.IndexByte(head, '('); open >= 0 {
		if !strings.HasSuffix(head, ")") {
			return "", false
		}
		head = head[:open]
	}
	if _, ok := conventionalBranchTypes[head]; !ok {
		return "", false
	}
	return head, true
}
