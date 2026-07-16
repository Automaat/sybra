package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RepairReport struct {
	QuarantinedRefs   []string
	PrunedWorktrees   bool
	RefetchedBranches []string
}

var QuarantineDir string

var badRefMarkers = []string{
	"fatal: bad object",
	"bad object head",
	"not a valid object name",
	"invalid object",
	"invalid revision range",
	"missing object",
	"unable to read sha1 file",
	"object file",
	"loose object",
	"unknown revision",
	"ambiguous argument",
	"reference broken",
}

func IsBadRefError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range badRefMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func CommonDir(ctx context.Context, worktreePath string) (string, error) {
	return gitCommonDir(ctx, worktreePath)
}

func RepairBareClone(ctx context.Context, barePath, taskBranch string) (RepairReport, error) {
	var report RepairReport
	err := withBareRepoLock(barePath, func() error {
		var innerErr error
		report, innerErr = repairBareCloneLocked(ctx, barePath, taskBranch)
		return innerErr
	})
	return report, err
}

func repairBareCloneLocked(ctx context.Context, barePath, taskBranch string) (RepairReport, error) {
	var report RepairReport

	refsOut, err := outputBare(ctx, barePath, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return report, fmt.Errorf("enumerate refs/heads: %w", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(refsOut), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		if checkErr := runBare(ctx, barePath, "cat-file", "-e", ref+"^{object}"); checkErr == nil {
			continue
		}
		badValue, _ := outputBare(ctx, barePath, "rev-parse", ref)
		QuarantineRef(barePath, ref, strings.TrimSpace(badValue))
		delErr := withLockRetry(func() error {
			return runBare(ctx, barePath, "update-ref", "-d", ref)
		})
		if delErr == nil {
			report.QuarantinedRefs = append(report.QuarantinedRefs, ref)
		}
	}

	worktreesDir := filepath.Join(barePath, "worktrees")
	entries, _ := os.ReadDir(worktreesDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wtRef := "worktrees/" + entry.Name() + "/HEAD"
		headBytes, readErr := os.ReadFile(filepath.Join(worktreesDir, entry.Name(), "HEAD"))
		if readErr != nil {
			continue
		}
		head := strings.TrimSpace(string(headBytes))
		target := head
		if trimmed, ok := strings.CutPrefix(head, "ref: "); ok {
			target = strings.TrimSpace(trimmed)
		}
		if checkErr := runBare(ctx, barePath, "rev-parse", "--verify", target+"^{commit}"); checkErr == nil {
			continue
		}
		QuarantineRef(barePath, wtRef, head)
		report.QuarantinedRefs = append(report.QuarantinedRefs, wtRef)
	}

	if len(report.QuarantinedRefs) > 0 {
		_ = withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "prune")
		})
		report.PrunedWorktrees = true
	}

	refspecs := []string{"+refs/heads/*:refs/remotes/origin/*"}
	if taskBranch != "" {
		refspecs = append(refspecs, "+refs/heads/"+taskBranch+":refs/remotes/origin/"+taskBranch)
	}
	fetchArgs := append([]string{"fetch", "origin"}, refspecs...)
	if fetchErr := runBare(ctx, barePath, fetchArgs...); fetchErr == nil {
		report.RefetchedBranches = refspecs
	}

	return report, nil
}

func QuarantineRef(barePath, ref, badValue string) {
	if QuarantineDir == "" {
		return
	}
	if err := os.MkdirAll(QuarantineDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(quarantineLogPath(barePath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s\t%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), barePath, ref, badValue)
}

func quarantineLogPath(barePath string) string {
	replacer := strings.NewReplacer("/", "_", string(filepath.Separator), "_")
	safe := replacer.Replace(filepath.Clean(barePath))
	return filepath.Join(QuarantineDir, safe+".log")
}
