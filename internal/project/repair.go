package project

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// cannot read.
//
// Split out of CheckBareCloneHealth because the two answer different
// questions and only one of them should gate dispatch. An index git cannot
// parse makes that one worktree unusable and is worth repairing; an index
// that parses but whose cache-tree names pruned blobs is inert. Deriving both
// from a single unscoped `git fsck` exit code conflated them, so ordinary
// worktree churn read as clone corruption.
//
// Reads the index via `git ls-files` rather than `git status`: it parses the
// index without stat-ing the working tree, which matters when this runs
// across dozens of live worktrees on every dispatch.
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
		if err := indexReadable(ctx, checkoutPath); err != nil {
			return fmt.Errorf("worktrees/%s/index file is unreadable: %w", entry.Name(), err)
		}
	}
	return nil
}

// indexReadable reports whether git can parse the worktree's index.
//
// Discards stdout rather than using executil.Run: this runs per worktree on
// the dispatch path, and ls-files on a large repo emits thousands of paths
// that would otherwise be buffered in full only to be thrown away. Only
// stderr is kept, since that is what explains a parse failure.
func indexReadable(ctx context.Context, checkoutPath string) error {
	cmd := exec.CommandContext(ctx, "git", "ls-files")
	cmd.Dir = checkoutPath
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

// objectCorruptionMarkers catch a worktree index/cache-tree referencing a
// blob/tree/commit object that has gone missing from the shared bare
// clone's object store — distinct from badRefMarkers, which catch a *ref*
// that fails to resolve. Here the ref resolves fine; an object it
// (transitively) points at is simply gone, which git surfaces from
// status/add/commit rather than from a ref lookup. "unable to read" and the
// fsck-style "broken link"/"missing blob" wording were added after a live
// checkpoint failure showed `fatal: unable to read <sha>` wasn't covered by
// badRefMarkers's more specific "unable to read sha1 file".
var objectCorruptionMarkers = []string{
	"unable to read",
	"broken link",
	"missing blob",
	"missing tree",
	"missing commit",
	"invalid sha1 path",
	"object corrupt or missing",
	"is corrupt",
}

func isObjectCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range objectCorruptionMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// repairWorktreeObjectStore re-syncs wtPath's shared bare clone object store
// with origin and refreshes the worktree's index, recovering from a
// checkpoint commit that failed with isObjectCorruptionError. A plain
// `fetch` only ever adds objects reachable from newly-advanced refs, which
// does nothing when the missing object is still reachable from a ref the
// bare clone already has — so this uses `--refetch` to force git to
// re-download every object regardless of what it already believes it has.
// `git reset --mixed HEAD` then drops any stale index/cache-tree entries
// (including ones pointing at now-repaired objects) without touching
// working-tree files, so uncommitted edits survive the repair.
func repairWorktreeObjectStore(ctx context.Context, wtPath string) error {
	barePath, err := gitCommonDir(ctx, wtPath)
	if err != nil {
		return fmt.Errorf("resolve git common dir: %w", err)
	}

	fetchErr := withBareRepoLock(barePath, func() error {
		removeCorruptLooseObjects(ctx, barePath)
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "fetch", "origin", "--refetch", "+refs/heads/*:refs/remotes/origin/*")
		})
	})
	if fetchErr != nil {
		return fmt.Errorf("refetch bare clone objects: %w", fetchErr)
	}

	reset := exec.CommandContext(ctx, "git", "reset", "--mixed", "HEAD")
	reset.Dir = wtPath
	reset.Env = cleanGitEnv()
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("git reset --mixed HEAD: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// corruptLooseObjectRe matches `git fsck --full`'s report for a loose object
// whose on-disk bytes fail to inflate, e.g.:
//
//	error: 45b983be...: object corrupt or missing: ./objects/45/b983be...
var corruptLooseObjectRe = regexp.MustCompile(`object corrupt or missing: (\S+)`)

// removeCorruptLooseObjects runs `git fsck --full` against barePath and
// deletes every loose object file it reports as corrupt. This must run
// before a `--refetch`: unpacking a fetched pack skips writing any object
// whose filename already exists on disk, without validating that the
// existing copy is actually readable — so a corrupt-but-present loose
// object silently survives a refetch unless it's removed first. Best-effort:
// fsck/removal failures are swallowed here since the caller's refetch+reset
// still recovers the (more common) genuinely-missing-object case on its own.
// Must be called with barePath's bare-repo lock already held.
func removeCorruptLooseObjects(ctx context.Context, barePath string) {
	cmd := exec.CommandContext(ctx, "git", "fsck", "--full")
	cmd.Dir = barePath
	cmd.Env = cleanGitEnv()
	out, _ := cmd.CombinedOutput()
	for _, m := range corruptLooseObjectRe.FindAllStringSubmatch(string(out), -1) {
		rel := strings.TrimPrefix(m[1], "./")
		_ = os.Remove(filepath.Join(barePath, filepath.FromSlash(rel)))
	}
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
