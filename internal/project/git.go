package project

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/executil"
	"gopkg.in/yaml.v3"
)

var (
	// ownerRe: alphanumeric + hyphens, 1-39 chars, no leading/trailing hyphen.
	ownerRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)
	// repoRe: alphanumeric + hyphens/underscores/dots, 1-100 chars.
	// "." and ".." are rejected by explicit check below.
	repoRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,100}$`)
)

// ErrBranchMissing is returned by PushSync when the local branch ref does not exist.
var ErrBranchMissing = errors.New("local branch ref does not exist")

// ErrBranchDiverged is returned by ReconcileWithRemote when the local branch and
// the remote branch head have genuinely diverged (neither is an ancestor of the
// other). Proceeding would force-push over remote-only commits — e.g. a fix
// pushed from another clone/machine — so callers must surface this rather than
// silently clobber the remote.
var ErrBranchDiverged = errors.New("local branch diverged from remote head")

// ErrDirtyWorktree is returned by ReconcileWithRemote when the worktree has
// uncommitted changes. Fast-forwarding HEAD under live, uncommitted edits can
// silently shift the author's base, so callers must fail closed instead.
var ErrDirtyWorktree = errors.New("worktree has uncommitted changes")

// ErrRemoteAdvanced is returned by PushSync when the live remote branch head no
// longer matches the remote-tracking ref the push decision was based on, or
// when the live remote head cannot be verified before deciding whether to push.
// It is wrapped together with ErrDivergedNeedsResolve only when the verified
// live remote state also requires agent-driven branch reconciliation.
var ErrRemoteAdvanced = errors.New("remote branch advanced past tracking ref")

// ErrDivergedNeedsResolve is returned by PushSync when the local branch and
// its remote tracking branch have diverged (neither is a fast-forward of the
// other). PushSync never force-pushes to resolve this — a force push rewrites
// already-published history, which Sybra must never do out-of-band. Callers
// must instead spawn agent work to reconcile the branches (rebase/merge onto
// the remote) so a later push can fast-forward.
var ErrDivergedNeedsResolve = errors.New("branch diverged from remote; needs agent-driven resolution, not a force push")

func runBare(ctx context.Context, barePath string, args ...string) error {
	return executil.Run(ctx, barePath, "git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

func outputBare(ctx context.Context, barePath string, args ...string) (string, error) {
	return executil.Output(ctx, barePath, "git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

// LoadRepoConfig reads .sybra.yaml from the worktree root. Returns an empty
// RepoConfig (not an error) if the file does not exist.
func LoadRepoConfig(worktreePath string) (*RepoConfig, error) {
	data, err := os.ReadFile(filepath.Join(worktreePath, ".sybra.yaml"))
	if os.IsNotExist(err) {
		return &RepoConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .sybra.yaml: %w", err)
	}
	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse .sybra.yaml: %w", err)
	}
	return &cfg, nil
}

// LoadRepoConfigAtDefaultBranch reads .sybra.yaml as tracked at the project's
// default branch in the bare clone, never from a checked-out worktree.
// Callers preparing a worktree for an untrusted ref (a PR head, possibly from
// a fork, or a Renovate branch) must use this instead of LoadRepoConfig: that
// checked-out ref's own .sybra.yaml is attacker-controlled, and its
// setup:/checks: commands run via `sh -c` outside the agent permission model
// (see LoadRepoConfig's callers in internal/worktree for the trusted case,
// where the checked-out branch is one Sybra created off this same default
// branch). Returns an empty RepoConfig (not an error) if the file is not
// tracked at that ref.
func LoadRepoConfigAtDefaultBranch(ctx context.Context, barePath string) (*RepoConfig, error) {
	branch, err := DefaultBranch(ctx, barePath)
	if err != nil {
		return nil, fmt.Errorf("resolve default branch: %w", err)
	}
	data, found, err := showFileAtRef(ctx, barePath, "refs/remotes/origin/"+branch, ".sybra.yaml")
	if errors.Is(err, errRefUnresolved) {
		// The remote-tracking ref is genuinely absent (e.g. never fetched) — fall
		// back to the local head, mirroring TrackedFilesAtDefaultBranch. Only a
		// ref-resolution failure triggers this: a transient error (context
		// cancellation, disk hiccup) must propagate rather than silently serve
		// the frozen refs/heads/<branch>, which review/fix worktrees never
		// advance and so may be stale.
		data, found, err = showFileAtRef(ctx, barePath, "refs/heads/"+branch, ".sybra.yaml")
	}
	if err != nil {
		return nil, fmt.Errorf("read .sybra.yaml at default branch %s: %w", branch, err)
	}
	if !found {
		return &RepoConfig{}, nil
	}
	var cfg RepoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse .sybra.yaml: %w", err)
	}
	return &cfg, nil
}

// errRefUnresolved marks a showFileAtRef failure where ref itself does not
// resolve (e.g. a remote-tracking ref that was never fetched). Callers use
// errors.Is to distinguish this recoverable "try another ref" case from a
// transient failure (context cancellation, disk error) that must propagate
// instead of silently falling back to a possibly-stale ref.
var errRefUnresolved = errors.New("ref does not resolve")

// showFileAtRef returns the bytes of path as tracked at ref in the bare repo
// via `git show`, without ever materializing a worktree checkout. found is
// false with a nil error when ref resolves but path is not tracked there —
// callers treat that the same as a missing file. A ref that does not resolve
// is returned wrapped in errRefUnresolved so the caller can try a fallback
// ref; any other failure (e.g. context cancellation) is returned as a plain
// error so it propagates rather than triggering a fallback.
func showFileAtRef(ctx context.Context, barePath, ref, path string) (data []byte, found bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "safe.bareRepository=all", "show", ref+":"+path)
	cmd.Dir = barePath
	out, runErr := cmd.Output()
	if runErr == nil {
		return out, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		stderr := string(exitErr.Stderr)
		switch {
		case strings.Contains(stderr, "does not exist in") || strings.Contains(stderr, "exists on disk, but not in"):
			return nil, false, nil
		case strings.Contains(stderr, "invalid object name") ||
			strings.Contains(stderr, "unknown revision") ||
			strings.Contains(stderr, "bad revision") ||
			strings.Contains(stderr, "ambiguous argument"):
			return nil, false, fmt.Errorf("git show %s:%s: %w: %w", ref, path, errRefUnresolved, runErr)
		}
	}
	return nil, false, fmt.Errorf("git show %s:%s: %w", ref, path, runErr)
}

// InstallHooks writes pre-commit and pre-push git hooks into the worktree's
// hooks directory. Existing hooks are overwritten. No-op if checks is nil or
// both slices are empty.
func InstallHooks(ctx context.Context, worktreePath string, checks *ChecksConfig) error {
	if checks == nil || (len(checks.PreCommit) == 0 && len(checks.PrePush) == 0) {
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("resolve git dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	write := func(name string, commands []string) error {
		if len(commands) == 0 {
			return nil
		}
		var sb strings.Builder
		sb.WriteString("#!/bin/sh\nset -e\nunset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY\n")
		for _, c := range commands {
			sb.WriteString(c)
			sb.WriteByte('\n')
		}
		path := filepath.Join(hooksDir, name)
		if err := os.WriteFile(path, []byte(sb.String()), 0o755); err != nil {
			return fmt.Errorf("write %s hook: %w", name, err)
		}
		return nil
	}

	if err := write("pre-commit", checks.PreCommit); err != nil {
		return err
	}
	return write("pre-push", checks.PrePush)
}

const signoffHook = `#!/bin/sh
# Auto-installed by Sybra. Guarantees a DCO Signed-off-by trailer on every
# commit so PRs never fail the DCO check when an agent forgets 'git commit -s'.
msg_file="$1"
sob=$(git var GIT_AUTHOR_IDENT | sed -n 's/^\(.*>\).*$/Signed-off-by: \1/p')
[ -z "$sob" ] && exit 0
git interpret-trailers --if-exists addIfDifferent --trailer "$sob" --in-place "$msg_file"
`

// InstallSignoffHook writes a prepare-commit-msg hook that guarantees a DCO
// Signed-off-by trailer on every commit made in the worktree. Agents commit
// via a plain git commit and don't reliably pass -s, so relying on the prompt
// instruction leaves unsigned commits that fail the kumahq/kuma DCO check.
// prepare-commit-msg is the only hook that fires on every commit — including
// merges and --no-verify, which bypass only pre-commit and commit-msg — so it
// is the single place sign-off can be enforced no matter how the commit is
// produced. Unlike InstallHooks it is unconditional and manages its own hooks
// dir, so it also covers projects with no pre-commit/pre-push checks. The hook
// lives in the git-common-dir and so covers every worktree of the clone; the
// write is idempotent.
func InstallSignoffHook(ctx context.Context, worktreePath string) error {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("resolve git dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}
	path := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(path, []byte(signoffHook), 0o755); err != nil {
		return fmt.Errorf("write prepare-commit-msg hook: %w", err)
	}
	pin := exec.CommandContext(ctx, "git", "config", "core.hooksPath", hooksDir)
	pin.Dir = worktreePath
	if err := pin.Run(); err != nil {
		return fmt.Errorf("pin core.hooksPath: %w", err)
	}
	return nil
}

// ParseGitHubURL extracts owner and repo from a GitHub URL in SSH form
// (git@github.com:owner/repo[.git]) or HTTPS form
// (https://github.com/owner/repo[.git]). Returns an error for any other
// host or a malformed owner/repo segment.
func ParseGitHubURL(raw string) (owner, repo string, err error) {
	raw = strings.TrimSpace(raw)

	// SSH: git@github.com:owner/repo.git
	if path, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		path = strings.TrimSuffix(path, "/")
		path = strings.TrimSuffix(path, ".git")
		return splitOwnerRepo(path)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse url: %w", err)
	}

	if u.Host != "github.com" {
		return "", "", fmt.Errorf("unsupported host: %s", u.Host)
	}

	path := strings.TrimPrefix(u.Path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	return splitOwnerRepo(path)
}

func splitOwnerRepo(path string) (owner, repo string, err error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid owner/repo path: %q", path)
	}
	owner, repo = parts[0], parts[1]
	if !ownerRe.MatchString(owner) {
		return "", "", fmt.Errorf("invalid GitHub owner %q", owner)
	}
	if repo == "." || repo == ".." || !repoRe.MatchString(repo) {
		return "", "", fmt.Errorf("invalid GitHub repo %q", repo)
	}
	return owner, repo, nil
}

// CloneBare runs `git clone --bare` for repoURL into destPath, then
// configures remote.origin.fetch with the standard refspec so subsequent
// `git fetch origin` calls actually update refs/remotes/origin/* (a bare
// clone otherwise leaves it empty).
func CloneBare(ctx context.Context, repoURL, destPath string) error {
	if err := executil.Run(ctx, "", "git", "clone", "--bare", repoURL, destPath); err != nil {
		return err
	}
	if err := InstallSignoffHook(ctx, destPath); err != nil {
		return fmt.Errorf("install signoff hook: %w", err)
	}
	// `git clone --bare` leaves remote.origin.fetch empty, so later `git fetch
	// origin` becomes a no-op against refs/remotes/origin/*. Configure the
	// standard refspec so fetches actually update tracking refs.
	return runBare(ctx, destPath, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
}

// DefaultBranch resolves barePath's HEAD symbolic ref (e.g.
// refs/heads/main) and returns just the branch name (main).
func DefaultBranch(ctx context.Context, barePath string) (string, error) {
	ref, err := outputBare(ctx, barePath, "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	// refs/heads/main → main
	return filepath.Base(ref), nil
}

// ListTrackedFiles returns every file path tracked at ref in the bare repo,
// via `git ls-tree -r --name-only`. An empty tree yields an empty (non-nil
// wanted) slice; a missing ref returns an error.
func ListTrackedFiles(ctx context.Context, barePath, ref string) ([]string, error) {
	out, err := outputBare(ctx, barePath, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	files := make([]string, 0)
	if out == "" {
		return files, nil
	}
	for l := range strings.SplitSeq(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// TrackedFilesAtDefaultBranch resolves barePath's default branch and returns
// the files tracked there, preferring the remote-tracking ref
// (refs/remotes/origin/<branch>) and falling back to the local ref
// (refs/heads/<branch>) if the tracking ref is absent. Bare clones don't keep
// local heads current on fetch (remote.origin.fetch only updates
// refs/remotes/origin/*, see CloneBare), so the tracking ref reflects pushed
// state most reliably; the local-head fallback covers a fresh clone that
// hasn't been fetched yet, where only refs/heads/* exist.
func TrackedFilesAtDefaultBranch(ctx context.Context, barePath string) ([]string, error) {
	branch, err := DefaultBranch(ctx, barePath)
	if err != nil {
		return nil, err
	}
	files, err := ListTrackedFiles(ctx, barePath, "refs/remotes/origin/"+branch)
	if err == nil {
		return files, nil
	}
	return ListTrackedFiles(ctx, barePath, "refs/heads/"+branch)
}

// FetchOrigin fetches origin's heads into barePath's refs/remotes/origin/*
// under the bare-repo lock, retrying transient git-fetch lock contention.
// Skips the actual network fetch when a prior call already refreshed this
// bare clone within FetchTTL (see git_lock.go) — checked under the same lock
// that serializes concurrent callers, so a burst of prepares against one repo
// pays for exactly one fetch.
func FetchOrigin(ctx context.Context, barePath string) error {
	return withBareRepoLock(barePath, func() error {
		if fetchIsFresh(barePath) {
			return nil
		}
		err := withLockRetry(func() error {
			// Explicit refspec heals bare repos cloned before remote.origin.fetch
			// was configured, where `git fetch origin` silently skipped updating
			// refs/remotes/origin/*.
			return runBare(ctx, barePath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
		})
		if err != nil {
			return err
		}
		markFetched(barePath)
		return nil
	})
}

// FetchPRHead fetches a pull request's head commit into a stable local ref and
// returns that ref. GitHub exposes every PR's head at refs/pull/<N>/head on the
// upstream remote — including PRs opened from forks, whose head branch never
// lands under refs/remotes/origin/*. Checking out the returned ref therefore
// works where refs/remotes/origin/<branch> does not.
func FetchPRHead(ctx context.Context, barePath string, prNumber int) (string, error) {
	// Store under refs/sybra/* rather than refs/remotes/origin/* so a real
	// upstream branch named pr/<N> (which FetchOrigin maps to
	// refs/remotes/origin/pr/<N>) cannot collide with the fetched PR head.
	localRef := fmt.Sprintf("refs/sybra/pr/%d", prNumber)
	refspec := fmt.Sprintf("+refs/pull/%d/head:%s", prNumber, localRef)
	err := withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "fetch", "origin", refspec)
		})
	})
	if err != nil {
		return "", err
	}
	return localRef, nil
}

// SyncLocalBranch fast-forwards refs/heads/<branch> in the bare clone to match
// refs/remotes/origin/<branch> if the local branch is strictly behind the remote.
// If the local branch has commits ahead of (or diverged from) origin, it is left
// unchanged so those local-only commits are preserved. This keeps refs/heads/<branch>
// usable as a base ref for WorktreeBaseRefHead mode — without this, a bare clone
// never updates refs/heads/* after the initial clone, so FetchOrigin alone cannot
// make head mode reflect current remote state.
func SyncLocalBranch(ctx context.Context, barePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		localRef := "refs/heads/" + branch
		remoteRef := "refs/remotes/origin/" + branch

		remoteSHA, remoteOK := resolveRef(ctx, barePath, remoteRef)
		if !remoteOK {
			return nil // remote ref absent, nothing to sync
		}

		localSHA, localOK := resolveRef(ctx, barePath, localRef)
		if !localOK {
			// local branch absent — create it at the remote SHA
			return withLockRetry(func() error {
				return runBare(ctx, barePath, "update-ref", localRef, remoteSHA)
			})
		}

		if localSHA == remoteSHA {
			return nil
		}

		// Fast-forward only: advance local if it is a strict ancestor of remote.
		// merge-base --is-ancestor exits 0 when local IS an ancestor; exits 1 when not.
		if runBare(ctx, barePath, "merge-base", "--is-ancestor", localSHA, remoteSHA) == nil {
			return withLockRetry(func() error {
				return runBare(ctx, barePath, "update-ref", localRef, remoteSHA)
			})
		}
		// local is not an ancestor (has local-only or diverged commits) — preserve
		return nil
	})
}

// resolveRef resolves a git ref in the bare repo. Returns ("", false) if the
// ref does not exist or cannot be resolved.
func resolveRef(ctx context.Context, barePath, ref string) (string, bool) {
	sha, err := outputBare(ctx, barePath, "rev-parse", "--verify", ref)
	return sha, err == nil
}

// AutoCommitUncommitted stages and commits any uncommitted changes in wtPath
// with the given message, as a safety net against work an agent finished but
// forgot to commit. Returns whether a commit was actually made (false when
// the tree was clean or a git step failed). Best-effort: git errors are
// swallowed, matching the SanitizeWorktree call site this was extracted
// from — callers must not depend on the commit having landed.
func AutoCommitUncommitted(ctx context.Context, wtPath, message string) bool {
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = wtPath
	statusOut, err := statusCmd.Output()
	if err != nil || len(strings.TrimSpace(string(statusOut))) == 0 {
		return false
	}
	add := exec.CommandContext(ctx, "git", "add", "-A")
	add.Dir = wtPath
	if err := add.Run(); err != nil {
		return false
	}
	// --no-verify skips repo pre-commit hooks (installed from .sybra.yaml) so a
	// failing hook can't defeat this recovery path. -c user.* supplies a
	// fallback identity for worktrees where the agent never configured one.
	commit := exec.CommandContext(ctx, "git",
		"-c", "user.name=Sybra",
		"-c", "user.email=sybra@localhost",
		"commit", "--no-verify", "--no-gpg-sign", "--signoff", "-m", message)
	commit.Dir = wtPath
	return commit.Run() == nil
}

// SanitizeWorktree cleans up worktree state that would confuse agents:
//   - aborts any stuck rebase/merge/cherry-pick
//   - deletes local branches that shadow remote refs (e.g. local "origin/main")
func SanitizeWorktree(ctx context.Context, wtPath string) error {
	// Abort stuck rebase if any.
	if _, err := os.Stat(rebaseStateDir(ctx, wtPath)); err == nil {
		clearRebaseState(ctx, wtPath)
	}

	// Abort stuck merge if any. No harm running this when no merge is in
	// progress — git just errors, which we ignore (best-effort, like the
	// rebase abort above).
	abort := exec.CommandContext(ctx, "git", "merge", "--abort")
	abort.Dir = wtPath
	_ = abort.Run()

	// Auto-commit any uncommitted changes before resetting. Agents are expected
	// to commit before finishing, but if they forget this preserves their work
	// on the branch rather than destroying it.
	AutoCommitUncommitted(ctx, wtPath, "wip: auto-commit uncommitted agent work\n\nSanitizeWorktree preserved uncommitted changes before reset.")

	// Discard any remaining working-tree dirt (e.g. ignored files, failed
	// commit) so the rebase can proceed cleanly. Committed work on the branch
	// is preserved.
	reset := exec.CommandContext(ctx, "git", "reset", "--hard", "HEAD")
	reset.Dir = wtPath
	_ = reset.Run()
	clean := exec.CommandContext(ctx, "git", "clean", "-fd")
	clean.Dir = wtPath
	_ = clean.Run()

	// Delete local branches that shadow remote tracking refs.
	// A local branch named "origin/foo" shadows "refs/remotes/origin/foo".
	listCmd := exec.CommandContext(ctx, "git", "branch", "--format=%(refname:short)")
	listCmd.Dir = wtPath
	branchOut, err := listCmd.Output()
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(branchOut)), "\n") {
		if !strings.HasPrefix(line, "origin/") {
			continue
		}
		del := exec.CommandContext(ctx, "git", "branch", "-D", line)
		del.Dir = wtPath
		_ = del.Run()
	}
	return nil
}

// ResetWorktreeForRetry discards partial work from a killed/hung agent before a
// bounded clean re-dispatch. Unlike SanitizeWorktree, it intentionally does not
// auto-commit dirty state: the retry must start from the pre-run baseline, not
// compound half-applied edits from the stopped process.
func ResetWorktreeForRetry(ctx context.Context, wtPath, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "HEAD"
	}

	if _, err := os.Stat(rebaseStateDir(ctx, wtPath)); err == nil {
		clearRebaseState(ctx, wtPath)
	}

	abort := exec.CommandContext(ctx, "git", "merge", "--abort")
	abort.Dir = wtPath
	_ = abort.Run()

	reset := exec.CommandContext(ctx, "git", "reset", "--hard", ref)
	reset.Dir = wtPath
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("reset worktree to %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	clean := exec.CommandContext(ctx, "git", "clean", "-fd")
	clean.Dir = wtPath
	if out, err := clean.CombinedOutput(); err != nil {
		return fmt.Errorf("clean worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clearRebaseState runs `git rebase --abort` and, if it fails (e.g. a
// corrupt or stale rebase-state dir git itself can't clean up), forcibly
// removes the rebase-state directory so it can't block the next git
// operation in this worktree. The abort runs on a context derived from ctx
// via context.WithoutCancel, bounded by its own timeout: if the caller's ctx
// is already canceled or its deadline expired, running the abort on that
// same ctx would skip cleanup and leave stale rebase state behind (mirrors
// RebaseOnto's cleanup contract, see rebaseAbortTimeout comment).
func clearRebaseState(ctx context.Context, wtPath string) {
	dir := rebaseStateDir(ctx, wtPath)
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebaseAbortTimeout)
	defer cancel()
	cmd := exec.CommandContext(abortCtx, "git", "rebase", "--abort")
	cmd.Dir = wtPath
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
	}
}

// rebaseStateDir returns the path to the rebase-merge or rebase-apply dir.
func rebaseStateDir(ctx context.Context, wtPath string) string {
	// git worktrees store rebase state inside the .git dir (which is a file
	// pointing to the actual gitdir). Use rev-parse to resolve it.
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return filepath.Join(wtPath, ".git", "rebase-merge")
	}
	gitDir := strings.TrimSpace(string(out))
	// Check both rebase-merge (interactive) and rebase-apply (am-style).
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		p := filepath.Join(gitDir, sub)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(gitDir, "rebase-merge")
}

// CreateWorktree creates a new branch off baseBranch and checks it out into
// worktreePath via `git worktree add -b`. For checking out an already
// existing branch use CreateWorktreeExisting; for a read-only detached
// checkout use CreateWorktreeDetached.
func CreateWorktree(ctx context.Context, barePath, worktreePath, branch, baseBranch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "add", worktreePath, "-b", branch, baseBranch)
		})
	})
}

// CreateWorktreeExisting checks out an existing branch into a new worktree.
func CreateWorktreeExisting(ctx context.Context, barePath, worktreePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "add", worktreePath, branch)
		})
	})
}

// BranchExists reports whether a local branch exists in the repo.
func BranchExists(ctx context.Context, barePath, branch string) bool {
	err := runBare(ctx, barePath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RefExists reports whether an arbitrary ref (e.g. refs/remotes/origin/<branch>)
// resolves in the repo.
func RefExists(ctx context.Context, barePath, ref string) bool {
	_, ok := resolveRef(ctx, barePath, ref)
	return ok
}

// CreateWorktreeDetached creates a worktree in detached HEAD mode from a remote ref.
// Used for read-only checkouts like code reviews.
func CreateWorktreeDetached(ctx context.Context, barePath, worktreePath, ref string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "add", "--detach", worktreePath, ref)
		})
	})
}

// ListWorktrees parses `git worktree list --porcelain` output for barePath
// into Worktree entries, excluding the bare repo's own record.
func ListWorktrees(ctx context.Context, barePath string) ([]Worktree, error) {
	out, err := outputBare(ctx, barePath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(out), nil
}

func parseWorktreePorcelain(raw string) []Worktree {
	var result []Worktree
	for block := range strings.SplitSeq(strings.TrimSpace(raw), "\n\n") {
		if strings.Contains(block, "\nbare") || strings.HasSuffix(block, "\nbare") {
			continue
		}
		var wt Worktree
		for line := range strings.SplitSeq(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "worktree "); ok {
				wt.Path = rest
			} else if rest, ok := strings.CutPrefix(line, "HEAD "); ok {
				if len(rest) > 7 {
					rest = rest[:7]
				}
				wt.Head = rest
			} else if ref, ok := strings.CutPrefix(line, "branch "); ok {
				branch, _ := strings.CutPrefix(ref, "refs/heads/")
				wt.Branch = branch
				wt.TaskID = taskIDFromBranch(wt.Branch)
			}
		}
		if wt.Path != "" {
			result = append(result, wt)
		}
	}
	return result
}

func taskIDFromBranch(branch string) string {
	if name, ok := strings.CutPrefix(branch, "sybra/"); ok {
		return trailingTaskID(name)
	}
	prefix, name, ok := strings.Cut(branch, "/")
	if !ok || !isSybraBranchPrefix(prefix) {
		return ""
	}
	return trailingTaskID(name)
}

func trailingTaskID(name string) string {
	if len(name) < 8 {
		return ""
	}
	id := name[len(name)-8:]
	if !isShortTaskID(id) {
		return ""
	}
	return id
}

func isShortTaskID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func isSybraBranchPrefix(prefix string) bool {
	switch prefix {
	case "feat", "fix", "docs", "style", "refactor", "perf", "test", "build", "ci", "chore", "revert":
		return true
	default:
		return false
	}
}

// PushRemote returns the remote that branch pushes should target. If the
// repository has a remote named "fork" (the user's fork of the upstream,
// typically configured manually or by `gh repo fork --remote`), returns
// "fork"; otherwise returns "origin". This lets sybra push agent branches to
// the fork — and `gh pr create` therefore open cross-repo PRs — without a
// project-level setting.
func PushRemote(ctx context.Context, repoPath string) string {
	if _, err := executil.Output(ctx, repoPath, "git", "config", "--get", "remote.fork.url"); err == nil {
		return "fork"
	}
	return "origin"
}

// forkOnlyDisabledPushURL is the sentinel pushURL written to origin when a
// fork remote exists. Agents using `git push origin <branch>` (with or
// without --no-verify) hit a transport-level failure naming this sentinel,
// so the error message itself documents the policy.
const forkOnlyDisabledPushURL = "sybra-disabled-do-not-push-to-upstream-use-fork"

// EnforceForkOnlyPush configures a worktree so pushes never reach origin
// when a "fork" remote exists. It overrides remote.origin.pushurl with a
// deliberately invalid value; git push fails before any network call, which
// also means --no-verify cannot bypass this guard (unlike a pre-push hook).
//
// When no fork remote exists, any previously written sentinel pushurl is
// cleared so single-remote workflows (pet projects without a fork) keep
// pushing to origin normally. Foreign pushurl values (set by the user) are
// left untouched.
func EnforceForkOnlyPush(ctx context.Context, worktreePath string) error {
	if _, err := executil.Output(ctx, worktreePath, "git", "config", "--get", "remote.fork.url"); err != nil {
		current, getErr := executil.Output(ctx, worktreePath, "git", "config", "--get", "remote.origin.pushurl")
		if getErr == nil && strings.TrimSpace(current) == forkOnlyDisabledPushURL {
			_ = executil.Run(ctx, worktreePath, "git", "config", "--unset", "remote.origin.pushurl")
		}
		return nil
	}
	return executil.Run(ctx, worktreePath, "git", "remote", "set-url", "--push", "origin", forkOnlyDisabledPushURL)
}

// PushUpstream pushes branch to the fork remote if present, else origin,
// with -u to set remote tracking.
func PushUpstream(ctx context.Context, worktreePath, branch string) error {
	return executil.Run(ctx, worktreePath, "git", "push", "-u", PushRemote(ctx, worktreePath), branch)
}

// SetBranchTo force-sets a branch ref in the bare clone to point at commit,
// creating the branch if it does not already exist. Used by best-of-N
// promotion to fast-forward the canonical task branch onto the winning
// attempt's HEAD — a `git branch -f` against the shared bare repo, never a
// push, so it can never rewrite already-published remote history.
func SetBranchTo(ctx context.Context, barePath, branch, commit string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "branch", "-f", branch, commit)
		})
	})
}

// DeleteBranch force-removes a local branch ref from the bare repo. Used by
// best-of-N attempt cleanup so a discarded attempt branch is not silently
// reused as the base for a later attempt with the same ID. Force-delete (`-D`)
// because a losing attempt branch is never merged into the canonical branch,
// so `git branch -d` would refuse it. No-op-safe: deleting a missing branch
// returns an error the caller logs but does not treat as fatal.
func DeleteBranch(ctx context.Context, barePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "branch", "-D", branch)
		})
	})
}

// BackupBranchRef points refs/sybra-backup/<branch> at the branch's current
// tip so a subsequent DeleteBranch does not lose the commits — they stay
// recoverable via the backup ref. Used before recreating a task's branch from
// a fresh base when merge-based conflict recovery is exhausted.
func BackupBranchRef(ctx context.Context, barePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "update-ref", "refs/sybra-backup/"+branch, "refs/heads/"+branch)
		})
	})
}

// ResolveBareRef resolves a git ref (e.g. "refs/heads/<branch>") to its SHA in
// the bare repo. Returns ("", false) if the ref does not exist. Exported
// wrapper around resolveRef for best-of-N promotion's divergence check.
func ResolveBareRef(ctx context.Context, barePath, ref string) (string, bool) {
	return resolveRef(ctx, barePath, ref)
}

// IsAncestorInBare reports whether ancestor is reachable from descendant in
// the bare repo's history. Returns false when either commit is unknown.
// Exported for best-of-N promotion's divergence check (isAncestor requires a
// worktree checkout; promotion only has the bare clone at this point).
func IsAncestorInBare(ctx context.Context, barePath, ancestor, descendant string) bool {
	return runBare(ctx, barePath, "merge-base", "--is-ancestor", ancestor, descendant) == nil
}

// IsWorktreeDirty reports whether a worktree has uncommitted changes
// (tracked or untracked). Exported for best-of-N promotion's fail-closed
// dirty-worktree check.
func IsWorktreeDirty(ctx context.Context, worktreePath string) (bool, error) {
	return worktreeDirty(ctx, worktreePath)
}

// HardResetWorktree resets a worktree's index and working tree to ref (a
// branch name or commit SHA) via `git reset --hard`. Used by best-of-N
// promotion to materialize the canonical worktree at the winning attempt's
// HEAD after SetBranchTo has already moved the shared branch ref.
func HardResetWorktree(ctx context.Context, worktreePath, ref string) error {
	return executil.Run(ctx, worktreePath, "git", "reset", "--hard", ref)
}

// HeadArg returns the `gh pr create --head` value for branch: a bare branch
// name when pushing to origin, or "fork-owner:branch" when a fork remote is
// configured — matching PushRemote's routing decision so the PR always
// points at the branch that was actually pushed.
func HeadArg(ctx context.Context, worktreePath, branch string) (string, error) {
	if PushRemote(ctx, worktreePath) != "fork" {
		return branch, nil
	}
	forkURL, err := executil.Output(ctx, worktreePath, "git", "config", "--get", "remote.fork.url")
	if err != nil {
		return "", fmt.Errorf("resolve fork remote url: %w", err)
	}
	owner, _, err := ParseGitHubURL(strings.TrimSpace(forkURL))
	if err != nil {
		return "", fmt.Errorf("parse fork remote url: %w", err)
	}
	return owner + ":" + branch, nil
}

// CurrentBranch returns the checked-out branch name for a worktree.
func CurrentBranch(ctx context.Context, worktreePath string) (string, error) {
	branch, err := executil.Output(ctx, worktreePath, "git", "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(branch), nil
}

// CurrentCommit returns the full SHA of HEAD for a worktree.
func CurrentCommit(ctx context.Context, worktreePath string) (string, error) {
	sha, err := executil.Output(ctx, worktreePath, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// isAncestor reports whether ancestor is reachable from descendant in the
// worktree's history. Returns false when either ref is unknown.
func isAncestor(ctx context.Context, worktreePath, ancestor, descendant string) bool {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = worktreePath
	return cmd.Run() == nil
}

// remoteBranchHead queries the live head SHA of branch on remote via ls-remote.
// Returns ("", nil) when the remote branch does not exist (never pushed).
func remoteBranchHead(ctx context.Context, worktreePath, remote, branch string) (string, error) {
	var out string
	err := withNetworkRetry(ctx, func() error {
		var runErr error
		out, runErr = executil.Output(ctx, worktreePath, "git", "ls-remote", remote, "refs/heads/"+branch)
		return runErr
	})
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	// Output is "<sha>\trefs/heads/<branch>"; take the first field.
	return strings.Fields(out)[0], nil
}

func remoteTrackingRef(ctx context.Context, worktreePath, remote, branch string) (string, bool) {
	sha, err := executil.Output(ctx, worktreePath, "git", "rev-parse", "--verify", "refs/remotes/"+remote+"/"+branch)
	return sha, err == nil
}

func refreshTrackingRef(ctx context.Context, worktreePath, remote, branch string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	fetchErr := withNetworkRetry(ctx, func() error {
		return withLockRetry(func() error {
			return executil.Run(ctx, worktreePath, "git", "fetch", remote, refspec)
		})
	})
	if fetchErr != nil && !strings.Contains(fetchErr.Error(), "couldn't find remote ref") {
		return fmt.Errorf("fetch %s %s: %w", remote, refspec, fetchErr)
	}
	return nil
}

// ReconcileWithRemote fast-forwards the worktree's checked-out branch to the
// remote branch head before a rebase, so remote-only commits (e.g. a review fix
// pushed from another clone or machine) are carried forward instead of dropped
// by a later rebase + force-push.
//
// A bare clone never advances refs/heads/* on fetch, so a reused worktree can
// check out a branch that is strictly behind its own remote head. Rebasing that
// stale branch onto the base and then force-pushing (PushSync's divergence path)
// silently overwrites the remote — and --force-with-lease cannot catch it,
// because the pre-rebase fetch already advanced the tracking ref to the very
// commit about to be destroyed.
//
// Behaviour by comparison of local HEAD to the live remote head:
//   - remote branch absent, or local == remote: no-op
//   - local ahead (remote is ancestor of local): no-op, local already contains it
//   - remote ahead (local is ancestor of remote): fast-forward local to remote
//   - diverged (neither is an ancestor): ErrBranchDiverged, caller must not force
func ReconcileWithRemote(ctx context.Context, worktreePath, branch string) error {
	remote := PushRemote(ctx, worktreePath)
	// Refresh the tracking ref from the live remote; fork remotes are not
	// covered by the earlier FetchOrigin. A first-push branch has no remote
	// head yet, so "couldn't find remote ref" is expected and not fatal — any
	// other failure (network/auth/remote misconfig) must propagate, since
	// continuing on stale history is exactly the data-loss scenario this
	// function exists to prevent.
	if err := refreshTrackingRef(ctx, worktreePath, remote, branch); err != nil {
		return err
	}

	// An absent branch (first push) means there is nothing to reconcile; any
	// other ls-remote failure must propagate rather than silently no-op.
	remoteSHA, err := remoteBranchHead(ctx, worktreePath, remote, branch)
	if err != nil {
		return fmt.Errorf("ls-remote %s %s: %w", remote, branch, err)
	}
	if remoteSHA == "" {
		return nil
	}

	localSHA, err := executil.Output(ctx, worktreePath, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if localSHA == remoteSHA {
		return nil
	}
	if isAncestor(ctx, worktreePath, remoteSHA, localSHA) {
		return nil // local already contains the remote head
	}
	if isAncestor(ctx, worktreePath, localSHA, remoteSHA) {
		dirty, err := worktreeDirty(ctx, worktreePath)
		if err != nil {
			return fmt.Errorf("check worktree dirty: %w", err)
		}
		if dirty {
			return ErrDirtyWorktree
		}
		// Remote strictly ahead — adopt its commits before rebasing.
		return executil.Run(ctx, worktreePath, "git", "merge", "--ff-only", remoteSHA)
	}
	return fmt.Errorf("%w: local %s vs remote %s/%s %s", ErrBranchDiverged, localSHA[:min(7, len(localSHA))], remote, branch, remoteSHA[:min(7, len(remoteSHA))])
}

func worktreeDirty(ctx context.Context, worktreePath string) (bool, error) {
	out, err := executil.Output(ctx, worktreePath, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// PushSync syncs the branch to the fork remote (if present) or origin using
// the minimum mode required:
//   - first push (remote tracking ref absent): regular push with -u
//   - local SHA == remote tracking SHA: no-op
//   - remote tracking SHA is an ancestor of local (fast-forward): regular push
//   - histories diverged: never force-pushes. Returns ErrDivergedNeedsResolve
//     so the caller can spawn agent work to reconcile the branches instead of
//     rewriting already-published history. If the live remote advanced since
//     the cached tracking ref, ErrRemoteAdvanced is wrapped too.
//
// Refreshes refs/remotes/<remote>/<branch> from the live remote before
// comparing, the same way ReconcileWithRemote does — a separate recovery
// worktree (e.g. branch-conflict-fix) can push this branch directly without
// ever touching this worktree's cached tracking ref, so comparing against
// that stale cache can see a divergence the live remote no longer has,
// re-triggering recovery in a loop even though it already succeeded.
//
// Returns ErrBranchMissing if the local branch ref does not exist.
func PushSync(ctx context.Context, worktreePath, branch string) error {
	if err := executil.Run(ctx, worktreePath, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return ErrBranchMissing
		}
		return err
	}

	remote := PushRemote(ctx, worktreePath)
	localSHA, err := executil.Output(ctx, worktreePath, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return err
	}

	beforeRefreshSHA, beforeRefreshOK := remoteTrackingRef(ctx, worktreePath, remote, branch)

	// Refresh the tracking ref first; a first-push branch has no remote head
	// yet, so "couldn't find remote ref" is expected and not fatal. Any other
	// failure (network/auth/remote misconfig) means the live remote state
	// can't be verified before a push decision — fail closed rather than
	// fall back to comparing against a possibly-stale cached ref.
	if err := refreshTrackingRef(ctx, worktreePath, remote, branch); err != nil {
		return fmt.Errorf("%w: could not verify live remote head before push: %w", ErrRemoteAdvanced, err)
	}

	remoteSHA, remoteOK := remoteTrackingRef(ctx, worktreePath, remote, branch)
	if !remoteOK {
		// Remote tracking ref unknown — first push, set upstream.
		return executil.Run(ctx, worktreePath, "git", "push", "-u", remote, branch)
	}
	remoteAdvanced := beforeRefreshOK && beforeRefreshSHA != remoteSHA

	if localSHA == remoteSHA {
		return nil
	}

	// Fast-forward when remote SHA is reachable from local SHA.
	if isAncestor(ctx, worktreePath, remoteSHA, localSHA) {
		return executil.Run(ctx, worktreePath, "git", "push", "-u", remote, branch)
	}

	// Divergence path: never force-push. remoteSHA reflects the freshly
	// fetched live remote head, so this is a genuine content divergence, not
	// a stale-cache artifact — the branch needs agent-driven resolution
	// (rebase/merge onto the remote), never a rewrite.
	if remoteAdvanced {
		return fmt.Errorf("%w: %w: local %s vs remote %s/%s %s diverged", ErrDivergedNeedsResolve, ErrRemoteAdvanced, localSHA[:min(7, len(localSHA))], remote, branch, remoteSHA[:min(7, len(remoteSHA))])
	}
	return fmt.Errorf("%w: local %s vs remote %s/%s %s diverged", ErrDivergedNeedsResolve, localSHA[:min(7, len(localSHA))], remote, branch, remoteSHA[:min(7, len(remoteSHA))])
}

// RemoveWorktree deletes worktreePath and its `git worktree` registration
// from barePath (`git worktree remove --force`), discarding any
// uncommitted changes in it. Use PruneWorktrees to clean up stale
// administrative entries left behind if the directory was already removed
// out-of-band.
func RemoveWorktree(ctx context.Context, barePath, worktreePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "remove", "--force", worktreePath)
		})
	})
}

// RemoveWorktreeReconcile removes a worktree directory, reconciling the
// orphan case where the directory exists on disk but its admin entry under
// the bare repo's worktrees/ dir is already gone (e.g. after a prior
// `worktree prune`, or a crash mid-cleanup). Plain `worktree remove --force`
// fails on an orphan ("not a git repository (null)") because git can't
// resolve its admin entry, leaving the directory behind to collide with the
// next `worktree add`. This tries the registered-removal path first, then
// prunes stale admin entries, then falls back to a raw directory removal if
// the path still exists.
func RemoveWorktreeReconcile(ctx context.Context, barePath, worktreePath string) error {
	// Registered case: this alone succeeds and removes the directory.
	_ = RemoveWorktree(ctx, barePath, worktreePath)
	// Drop stale admin entries so a subsequent `worktree add` doesn't trip
	// over a registration that no longer has a live directory either.
	_ = PruneWorktrees(ctx, barePath)
	_, statErr := os.Stat(worktreePath)
	switch {
	case statErr == nil:
		// Orphan case: the directory survived because git couldn't resolve
		// its admin entry. Fall back to a raw removal.
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("remove orphan worktree %s: %w", worktreePath, err)
		}
		_ = PruneWorktrees(ctx, barePath)
	case !os.IsNotExist(statErr):
		return fmt.Errorf("stat worktree %s: %w", worktreePath, statErr)
	}
	return nil
}

// PruneWorktrees removes stale worktree admin entries from the bare repo.
func PruneWorktrees(ctx context.Context, barePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "prune")
		})
	})
}

// WorktreeHealthy reports whether a checked-out worktree's git metadata is
// resolvable. A `false` result typically means the .git pointer references a
// path that no longer exists — the usual cause is a container redeploy that
// changed the in-container mount point of the bare clone.
func WorktreeHealthy(ctx context.Context, worktreePath string) bool {
	_, err := executil.Output(ctx, worktreePath, "git", "rev-parse", "--git-dir")
	return err == nil
}

// RepairWorktrees runs `git worktree repair` against the bare repo, which
// rewrites the absolute path back-pointers in every checked-out worktree's
// .git file and the bare's worktrees/<id>/gitdir file. Idempotent.
func RepairWorktrees(ctx context.Context, barePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(ctx, barePath, "worktree", "repair")
		})
	})
}

// rebaseAbortTimeout bounds the `git rebase --abort` cleanup below. It must
// not inherit the caller's ctx: if the rebase failed because ctx was
// canceled or its deadline expired, running the abort on that same ctx would
// skip cleanup and leave .git/rebase-* state behind in the worktree.
const rebaseAbortTimeout = 30 * time.Second

// RebaseOnto rebases the worktree's current branch onto the given ref.
// Aborts and returns an error on conflict.
//
// Skips the rebase entirely when ref is already an ancestor of HEAD (e.g. a
// prior merge already brought ref's history in, as recoverBranchConflictNoPR
// does). Plain `git rebase` linearizes history: it drops merge commits and
// replays HEAD's own pre-merge commits individually onto ref, which
// re-triggers the exact content conflict the merge just resolved even though
// ref's tip is fully contained in HEAD. `git merge-base --is-ancestor`
// considers ref merged in this case, so treating that as "nothing to do"
// avoids the identical conflict resurfacing on every subsequent prepare.
func RebaseOnto(ctx context.Context, worktreePath, ref string) error {
	if isAncestor(ctx, worktreePath, ref, "HEAD") {
		return nil
	}
	if err := executil.Run(ctx, worktreePath, "git", "rebase", ref); err != nil {
		abortCtx, cancel := context.WithTimeout(context.Background(), rebaseAbortTimeout)
		_ = executil.Run(abortCtx, worktreePath, "git", "rebase", "--abort") //nolint:contextcheck // detached cleanup must survive a cancelled caller ctx, see rebaseAbortTimeout comment
		cancel()
		return fmt.Errorf("rebase onto %s: %w", ref, err)
	}
	return nil
}

// MergeOnto merges ref into the worktree's current branch. Unlike RebaseOnto,
// it never rewrites existing commits — the branch's prior history is
// preserved and any subsequent push is a plain fast-forward, never a
// force-push. Aborts and returns an error on conflict. The abort runs on a
// context derived from ctx via context.WithoutCancel, bounded by its own
// timeout: if the merge failed because ctx was canceled or its deadline
// expired, running the abort on that same ctx would skip cleanup and leave
// MERGE_HEAD state behind in the worktree (mirrors RebaseOnto's cleanup
// contract). -c user.name/user.email/commit.gpgsign=false mirror
// AutoCommitUncommitted's fallback identity for worktrees where the agent
// never configured one; --no-verify skips repo pre-commit hooks the same
// way, so a failing hook can't defeat this recovery path.
func MergeOnto(ctx context.Context, worktreePath, ref string) error {
	if err := executil.Run(ctx, worktreePath, "git",
		"-c", "user.name=Sybra",
		"-c", "user.email=sybra@localhost",
		"-c", "commit.gpgsign=false",
		"merge", "--no-edit", "--no-verify", "--signoff", ref); err != nil {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebaseAbortTimeout)
		_ = executil.Run(abortCtx, worktreePath, "git", "merge", "--abort")
		cancel()
		return fmt.Errorf("merge %s: %w", ref, err)
	}
	return nil
}

// CleanMergeResult is the outcome of TryCleanMerge.
type CleanMergeResult int

const (
	// CleanMergeCreated: the merge succeeded with no conflicts and produced a
	// new commit (fast-forward or a merge commit) — the branch moved.
	CleanMergeCreated CleanMergeResult = iota
	// CleanMergeNoop: the merge succeeded but produced no new commit — the
	// branch was already up to date with baseRef.
	CleanMergeNoop
	// CleanMergeConflict: the merge reported conflicting hunks. The worktree
	// is left clean (merge aborted, any leftover state reset) before return.
	CleanMergeConflict
)

func (r CleanMergeResult) String() string {
	switch r {
	case CleanMergeCreated:
		return "created"
	case CleanMergeNoop:
		return "noop"
	case CleanMergeConflict:
		return "conflict"
	default:
		return fmt.Sprintf("CleanMergeResult(%d)", int(r))
	}
}

// TryCleanMerge attempts a deterministic `git merge baseRef` in wtPath and
// classifies the result without ever leaving the worktree in a conflicted or
// dirty state. It never pushes.
//
// baseRef is validated up front so an unresolvable ref surfaces as an error
// rather than being fed to `git merge`, which would otherwise fail with a
// less specific message. On conflict, the merge is aborted on a context
// derived via context.WithoutCancel (mirrors MergeOnto/RebaseOnto: cleanup
// must survive a caller ctx that was cancelled or deadlined out, or
// MERGE_HEAD state is left behind), then the worktree is verified clean via
// `git status --porcelain`; if not, it falls back to a hard reset to the
// pre-merge HEAD plus `git clean -fd` so the caller's agent fallback always
// starts from a clean tree.
func TryCleanMerge(ctx context.Context, wtPath, baseRef string) (CleanMergeResult, error) {
	if err := executil.Run(ctx, wtPath, "git", "rev-parse", "--verify", baseRef); err != nil {
		return CleanMergeConflict, fmt.Errorf("resolve base ref %s: %w", baseRef, err)
	}

	preMergeHEAD, err := executil.Output(ctx, wtPath, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return CleanMergeConflict, fmt.Errorf("resolve pre-merge HEAD: %w", err)
	}
	preMergeHEAD = strings.TrimSpace(preMergeHEAD)

	mergeErr := executil.Run(ctx, wtPath, "git",
		"-c", "user.name=Sybra",
		"-c", "user.email=sybra@localhost",
		"-c", "commit.gpgsign=false",
		"merge", "--no-edit", "--no-verify", "--signoff", baseRef)
	if mergeErr != nil {
		conflict := false
		var exitErr *exec.ExitError
		if errors.As(mergeErr, &exitErr) && exitErr.ExitCode() == 1 {
			conflict = true
		}

		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebaseAbortTimeout)
		defer cancel()
		_ = executil.Run(abortCtx, wtPath, "git", "merge", "--abort")

		statusOut, statusErr := executil.Output(abortCtx, wtPath, "git", "status", "--porcelain")
		if statusErr != nil || strings.TrimSpace(statusOut) != "" {
			_ = executil.Run(abortCtx, wtPath, "git", "reset", "--hard", preMergeHEAD)
			_ = executil.Run(abortCtx, wtPath, "git", "clean", "-fd")
		}
		if !conflict {
			return CleanMergeConflict, fmt.Errorf("merge %s into worktree: %w", baseRef, mergeErr)
		}
		return CleanMergeConflict, nil
	}

	postMergeHEAD, err := executil.Output(ctx, wtPath, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return CleanMergeConflict, fmt.Errorf("resolve post-merge HEAD: %w", err)
	}
	if strings.TrimSpace(postMergeHEAD) == preMergeHEAD {
		return CleanMergeNoop, nil
	}
	return CleanMergeCreated, nil
}
