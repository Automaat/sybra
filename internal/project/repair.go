package project

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type RepairReport struct {
	QuarantinedRefs        []string
	PrunedWorktrees        bool
	RebuiltWorktreeIndexes []string
	RefetchedBranches      []string
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

// fsckRefBatch bounds how many ref tips are passed to one `git fsck`
// invocation, so a repo with thousands of refs cannot overflow the argv limit.
const fsckRefBatch = 256

// CheckBareCloneHealth reports whether the object database is intact for
// everything reachable from refs.
//
// It scopes fsck to the ref tips rather than running it bare. An unscoped
// `git fsck` also walks every linked worktree's index and HEAD reflog, and
// those go stale as a matter of normal operation: a worktree reflog keeps
// pointing at objects a later prune removed, and an index cache-tree keeps
// naming blobs from a discarded state. Neither is reachable from any ref and
// neither affects what an agent can check out, but both make fsck exit
// non-zero forever.
//
// Measured on the deploy host with 40 live worktrees: 141 errors, of which
// 115 were stale worktree reflog entries and 26 were worktree index
// cache-tree references — while all 33,629 ref-reachable objects were
// present. The unscoped check therefore failed permanently on a healthy
// clone, and because the failure is permanent it could never clear: the
// agent-start circuit breaker tripped and parked tasks human-required with
// "bare clone health check failed" that no retry could resolve.
func CheckBareCloneHealth(ctx context.Context, barePath string) error {
	refs, err := bareRefTips(ctx, barePath)
	if err != nil {
		return fmt.Errorf("bare clone health check failed: %w", err)
	}
	if len(refs) == 0 {
		// No refs to scope to (fresh or fully quarantined clone). Nothing has
		// been checked out yet either, so the unscoped walk has no worktree
		// state to trip over.
		if err := runBare(ctx, barePath, "fsck", "--no-dangling"); err != nil {
			return fmt.Errorf("bare clone health check failed: %w", err)
		}
		return nil
	}
	for start := 0; start < len(refs); start += fsckRefBatch {
		end := min(start+fsckRefBatch, len(refs))
		args := append([]string{"fsck", "--no-dangling", "--no-reflogs"}, refs[start:end]...)
		if err := runBare(ctx, barePath, args...); err != nil {
			return fmt.Errorf("bare clone health check failed: %w", err)
		}
	}
	return nil
}

// CheckWorktreeIndexes reports the first linked worktree whose index git
// cannot use.
//
// Split out of CheckBareCloneHealth because the two answer different
// questions and only one of them should gate dispatch. A damaged index makes
// its own worktree unusable and is worth repairing, but it says nothing about
// whether the shared clone is fit for every other task. Deriving both from a
// single unscoped `git fsck` exit code conflated them, so ordinary worktree
// churn read as clone corruption.
//
// Note the reflog case stays out of scope on purpose: a worktree reflog naming
// a pruned object is genuinely inert, and folding it in is what made the old
// gate fail permanently. An index naming a missing object is not inert — it
// breaks the next commit — so indexUsable treats it as repair-worthy. See
// indexUsable for both probes.
func CheckWorktreeIndexes(ctx context.Context, barePath string) error {
	entries, err := os.ReadDir(filepath.Join(barePath, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			// A clone with no linked worktrees has no indexes to check.
			return nil
		}
		// Any other read failure (permissions, IO) is not evidence of health.
		return fmt.Errorf("read worktrees dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		adminDir := filepath.Join(barePath, "worktrees", entry.Name())
		checkoutPath := worktreeCheckoutPath(adminDir)
		if checkoutPath == "" {
			continue
		}
		if _, err := os.Stat(checkoutPath); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(adminDir, "index")); err != nil {
			continue
		}
		if err := indexUsable(ctx, checkoutPath); err != nil {
			return fmt.Errorf("worktrees/%s: %w", entry.Name(), err)
		}
	}
	return nil
}

// indexUsable reports whether git can both parse the worktree's index and
// resolve every object it names.
//
// Two probes, because they fail differently and only one was covered before:
// ls-files catches an index git cannot parse at all, while write-tree catches
// an index that parses but names a blob the object database no longer has.
// The second is the shape behind the "codegen gate could not checkpoint:
// recovery commit: invalid object <sha> for '<path>'" failures seen on the
// board — write-tree is the operation a commit performs, so it reproduces the
// break exactly rather than approximating it.
//
// Missing that second case was not merely incomplete reporting:
// rebuildWorktreeIndexes early-returns when this check passes, so an index in
// that state received no repair at all and the checkpoint kept failing.
//
// This runs on the repair path (rebuildWorktreeIndexes), not per dispatch, so
// write-tree's cost and its unreferenced-tree side effect are acceptable —
// the tree it writes is one a commit would have written anyway, and it is
// unreferenced garbage that gc collects.
func indexUsable(ctx context.Context, checkoutPath string) error {
	if err := runQuietGit(ctx, checkoutPath, "ls-files"); err != nil {
		return fmt.Errorf("index is unparseable: %w", err)
	}
	if err := runQuietGit(ctx, checkoutPath, "write-tree"); err != nil {
		return fmt.Errorf("index names an unresolvable object: %w", err)
	}
	return nil
}

// runQuietGit runs git in dir, discarding stdout and surfacing only stderr,
// which is the part that explains a failure.
func runQuietGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// bareRefTips lists the object ids every ref points at. They are the roots
// the scoped fsck walks from.
func bareRefTips(ctx context.Context, barePath string) ([]string, error) {
	out, err := outputBare(ctx, barePath, "rev-parse", "--all")
	if err != nil {
		return nil, fmt.Errorf("list ref tips: %w", err)
	}
	var refs []string
	for line := range strings.SplitSeq(out, "\n") {
		if tip := strings.TrimSpace(line); tip != "" {
			refs = append(refs, tip)
		}
	}
	return refs, nil
}

// bareCloneAndWorktreesHealthy covers both the object database and the linked
// worktree indexes, for callers that want the union rather than the dispatch
// gate alone.
//
// Production index repair does not run through here: FetchOrigin calls
// CheckBareCloneHealth and, on failure, repairBareCloneLocked, which reaches
// rebuildWorktreeIndexes -> CheckWorktreeIndexes. repairCheckpointWorktree is
// the other live entry point. EnsureBareCloneHealthy below has no non-test
// callers today — that predates this change and is left alone rather than
// deleted as unrelated scope.
func bareCloneAndWorktreesHealthy(ctx context.Context, barePath string) error {
	if err := CheckBareCloneHealth(ctx, barePath); err != nil {
		return err
	}
	return CheckWorktreeIndexes(ctx, barePath)
}

func EnsureBareCloneHealthy(ctx context.Context, barePath, taskBranch string) (RepairReport, error) {
	var report RepairReport
	err := withBareRepoLock(barePath, func() error {
		if err := bareCloneAndWorktreesHealthy(ctx, barePath); err == nil {
			return nil
		}
		var repairErr error
		report, repairErr = repairBareCloneLocked(ctx, barePath, taskBranch)
		if repairErr != nil {
			return repairErr
		}
		return bareCloneAndWorktreesHealthy(ctx, barePath)
	})
	return report, err
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

	releaseDeadWorktreeRefs(ctx, barePath, &report)
	rebuildWorktreeIndexes(ctx, barePath, &report)

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

	if len(report.QuarantinedRefs) > 0 {
		report.PrunedWorktrees = true
	}

	refspecs := []string{"+refs/heads/*:refs/remotes/origin/*"}
	if taskBranch != "" {
		refspecs = append(refspecs, "+refs/heads/"+taskBranch+":refs/remotes/origin/"+taskBranch)
	}
	fetchArgs := append([]string{"fetch", "origin"}, refspecs...)
	if fetchErr := runBareFetch(ctx, barePath, fetchArgs...); fetchErr == nil {
		report.RefetchedBranches = refspecs
	}

	return report, nil
}

func rebuildWorktreeIndexes(ctx context.Context, barePath string, report *RepairReport) {
	if CheckWorktreeIndexes(ctx, barePath) == nil {
		return
	}
	worktreesDir := filepath.Join(barePath, "worktrees")
	entries, _ := os.ReadDir(worktreesDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wtAdminDir := filepath.Join(worktreesDir, entry.Name())
		checkoutPath := worktreeCheckoutPath(wtAdminDir)
		if checkoutPath == "" {
			continue
		}
		if _, err := os.Stat(checkoutPath); err != nil {
			continue
		}
		headBytes, err := os.ReadFile(filepath.Join(wtAdminDir, "HEAD"))
		if err != nil {
			continue
		}
		head := strings.TrimSpace(string(headBytes))
		target := head
		if trimmed, ok := strings.CutPrefix(head, "ref: "); ok {
			target = strings.TrimSpace(trimmed)
		}
		if checkErr := runBare(ctx, barePath, "rev-parse", "--verify", target+"^{commit}"); checkErr != nil {
			continue
		}
		indexPath := filepath.Join(wtAdminDir, "index")
		if _, err := os.Stat(indexPath); err != nil {
			continue
		}
		quarantined := indexPath + ".sybra-quarantine-" + time.Now().UTC().Format("20060102T150405.000000000")
		if err := os.Rename(indexPath, quarantined); err != nil {
			continue
		}
		QuarantineRef(barePath, "worktrees/"+entry.Name()+"/index", quarantined)
		cmd := exec.CommandContext(ctx, "git", "-C", checkoutPath, "reset", "--mixed", "HEAD")
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.Rename(quarantined, indexPath)
			_ = out
			continue
		}
		report.RebuiltWorktreeIndexes = append(report.RebuiltWorktreeIndexes, "worktrees/"+entry.Name()+"/index")
	}
}

func releaseDeadWorktreeRefs(ctx context.Context, barePath string, report *RepairReport) {
	worktreesDir := filepath.Join(barePath, "worktrees")
	entries, _ := os.ReadDir(worktreesDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wtAdminDir := filepath.Join(worktreesDir, entry.Name())
		wtRef := "worktrees/" + entry.Name() + "/HEAD"
		headBytes, readErr := os.ReadFile(filepath.Join(wtAdminDir, "HEAD"))
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
		if checkoutPath := worktreeCheckoutPath(wtAdminDir); checkoutPath != "" {
			_ = withLockRetry(func() error {
				return runBare(ctx, barePath, "worktree", "remove", "--force", checkoutPath)
			})
		}
	}
	_ = withLockRetry(func() error {
		return runBare(ctx, barePath, "worktree", "prune")
	})
}

func worktreeCheckoutPath(adminDir string) string {
	gitdirBytes, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return ""
	}
	gitdirPath := strings.TrimSpace(string(gitdirBytes))
	return strings.TrimSuffix(gitdirPath, string(filepath.Separator)+".git")
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
