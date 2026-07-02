package project

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

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

// ErrRemoteAdvanced is returned by PushSync when the live remote branch head no
// longer matches the remote-tracking ref the push decision was based on. The
// tracking ref is stale, so a --force-with-lease would clobber commits pushed
// after the last fetch.
var ErrRemoteAdvanced = errors.New("remote branch advanced past tracking ref")

func runBare(barePath string, args ...string) error {
	return executil.Run(barePath, "git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

func outputBare(barePath string, args ...string) (string, error) {
	return executil.Output(barePath, "git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
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

// InstallHooks writes pre-commit and pre-push git hooks into the worktree's
// hooks directory. Existing hooks are overwritten. No-op if checks is nil or
// both slices are empty.
func InstallHooks(worktreePath string, checks *ChecksConfig) error {
	if checks == nil || (len(checks.PreCommit) == 0 && len(checks.PrePush) == 0) {
		return nil
	}

	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
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
func CloneBare(repoURL, destPath string) error {
	if err := executil.Run("", "git", "clone", "--bare", repoURL, destPath); err != nil {
		return err
	}
	// `git clone --bare` leaves remote.origin.fetch empty, so later `git fetch
	// origin` becomes a no-op against refs/remotes/origin/*. Configure the
	// standard refspec so fetches actually update tracking refs.
	return runBare(destPath, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
}

// DefaultBranch resolves barePath's HEAD symbolic ref (e.g.
// refs/heads/main) and returns just the branch name (main).
func DefaultBranch(barePath string) (string, error) {
	ref, err := outputBare(barePath, "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	// refs/heads/main → main
	return filepath.Base(ref), nil
}

// ListTrackedFiles returns every file path tracked at ref in the bare repo,
// via `git ls-tree -r --name-only`. An empty tree yields an empty (non-nil
// wanted) slice; a missing ref returns an error.
func ListTrackedFiles(barePath, ref string) ([]string, error) {
	out, err := outputBare(barePath, "ls-tree", "-r", "--name-only", ref)
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
func TrackedFilesAtDefaultBranch(barePath string) ([]string, error) {
	branch, err := DefaultBranch(barePath)
	if err != nil {
		return nil, err
	}
	files, err := ListTrackedFiles(barePath, "refs/remotes/origin/"+branch)
	if err == nil {
		return files, nil
	}
	return ListTrackedFiles(barePath, "refs/heads/"+branch)
}

// FetchOrigin fetches origin's heads into barePath's refs/remotes/origin/*
// under the bare-repo lock, retrying transient git-fetch lock contention.
func FetchOrigin(barePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			// Explicit refspec heals bare repos cloned before remote.origin.fetch
			// was configured, where `git fetch origin` silently skipped updating
			// refs/remotes/origin/*.
			return runBare(barePath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
		})
	})
}

// FetchPRHead fetches a pull request's head commit into a stable local ref and
// returns that ref. GitHub exposes every PR's head at refs/pull/<N>/head on the
// upstream remote — including PRs opened from forks, whose head branch never
// lands under refs/remotes/origin/*. Checking out the returned ref therefore
// works where refs/remotes/origin/<branch> does not.
func FetchPRHead(barePath string, prNumber int) (string, error) {
	// Store under refs/sybra/* rather than refs/remotes/origin/* so a real
	// upstream branch named pr/<N> (which FetchOrigin maps to
	// refs/remotes/origin/pr/<N>) cannot collide with the fetched PR head.
	localRef := fmt.Sprintf("refs/sybra/pr/%d", prNumber)
	refspec := fmt.Sprintf("+refs/pull/%d/head:%s", prNumber, localRef)
	err := withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "fetch", "origin", refspec)
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
func SyncLocalBranch(barePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		localRef := "refs/heads/" + branch
		remoteRef := "refs/remotes/origin/" + branch

		remoteSHA, remoteOK := resolveRef(barePath, remoteRef)
		if !remoteOK {
			return nil // remote ref absent, nothing to sync
		}

		localSHA, localOK := resolveRef(barePath, localRef)
		if !localOK {
			// local branch absent — create it at the remote SHA
			return withLockRetry(func() error {
				return runBare(barePath, "update-ref", localRef, remoteSHA)
			})
		}

		if localSHA == remoteSHA {
			return nil
		}

		// Fast-forward only: advance local if it is a strict ancestor of remote.
		// merge-base --is-ancestor exits 0 when local IS an ancestor; exits 1 when not.
		if runBare(barePath, "merge-base", "--is-ancestor", localSHA, remoteSHA) == nil {
			return withLockRetry(func() error {
				return runBare(barePath, "update-ref", localRef, remoteSHA)
			})
		}
		// local is not an ancestor (has local-only or diverged commits) — preserve
		return nil
	})
}

// resolveRef resolves a git ref in the bare repo. Returns ("", false) if the
// ref does not exist or cannot be resolved.
func resolveRef(barePath, ref string) (string, bool) {
	sha, err := outputBare(barePath, "rev-parse", "--verify", ref)
	return sha, err == nil
}

// AutoCommitUncommitted stages and commits any uncommitted changes in wtPath
// with the given message, as a safety net against work an agent finished but
// forgot to commit. Returns whether a commit was actually made (false when
// the tree was clean or a git step failed). Best-effort: git errors are
// swallowed, matching the SanitizeWorktree call site this was extracted
// from — callers must not depend on the commit having landed.
func AutoCommitUncommitted(wtPath, message string) bool {
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtPath
	statusOut, err := statusCmd.Output()
	if err != nil || len(strings.TrimSpace(string(statusOut))) == 0 {
		return false
	}
	add := exec.Command("git", "add", "-A")
	add.Dir = wtPath
	if err := add.Run(); err != nil {
		return false
	}
	// --no-verify skips repo pre-commit hooks (installed from .sybra.yaml) so a
	// failing hook can't defeat this recovery path. -c user.* supplies a
	// fallback identity for worktrees where the agent never configured one.
	commit := exec.Command("git",
		"-c", "user.name=Sybra",
		"-c", "user.email=sybra@localhost",
		"commit", "--no-verify", "--no-gpg-sign", "-m", message)
	commit.Dir = wtPath
	return commit.Run() == nil
}

// SanitizeWorktree cleans up worktree state that would confuse agents:
//   - aborts any stuck rebase/merge/cherry-pick
//   - deletes local branches that shadow remote refs (e.g. local "origin/main")
func SanitizeWorktree(wtPath string) error {
	// Abort stuck rebase if any.
	if _, err := os.Stat(rebaseStateDir(wtPath)); err == nil {
		cmd := exec.Command("git", "rebase", "--abort")
		cmd.Dir = wtPath
		_ = cmd.Run() // best-effort
	}

	// Abort stuck merge if any. No harm running this when no merge is in
	// progress — git just errors, which we ignore (best-effort, like the
	// rebase abort above).
	abort := exec.Command("git", "merge", "--abort")
	abort.Dir = wtPath
	_ = abort.Run()

	// Auto-commit any uncommitted changes before resetting. Agents are expected
	// to commit before finishing, but if they forget this preserves their work
	// on the branch rather than destroying it.
	AutoCommitUncommitted(wtPath, "wip: auto-commit uncommitted agent work\n\nSanitizeWorktree preserved uncommitted changes before reset.")

	// Discard any remaining working-tree dirt (e.g. ignored files, failed
	// commit) so the rebase can proceed cleanly. Committed work on the branch
	// is preserved.
	reset := exec.Command("git", "reset", "--hard", "HEAD")
	reset.Dir = wtPath
	_ = reset.Run()
	clean := exec.Command("git", "clean", "-fd")
	clean.Dir = wtPath
	_ = clean.Run()

	// Delete local branches that shadow remote tracking refs.
	// A local branch named "origin/foo" shadows "refs/remotes/origin/foo".
	listCmd := exec.Command("git", "branch", "--format=%(refname:short)")
	listCmd.Dir = wtPath
	branchOut, err := listCmd.Output()
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(branchOut)), "\n") {
		if !strings.HasPrefix(line, "origin/") {
			continue
		}
		del := exec.Command("git", "branch", "-D", line)
		del.Dir = wtPath
		_ = del.Run()
	}
	return nil
}

// ResetWorktreeForRetry discards partial work from a killed/hung agent before a
// bounded clean re-dispatch. Unlike SanitizeWorktree, it intentionally does not
// auto-commit dirty state: the retry must start from the pre-run baseline, not
// compound half-applied edits from the stopped process.
func ResetWorktreeForRetry(wtPath, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "HEAD"
	}

	if _, err := os.Stat(rebaseStateDir(wtPath)); err == nil {
		cmd := exec.Command("git", "rebase", "--abort")
		cmd.Dir = wtPath
		_ = cmd.Run()
	}

	abort := exec.Command("git", "merge", "--abort")
	abort.Dir = wtPath
	_ = abort.Run()

	reset := exec.Command("git", "reset", "--hard", ref)
	reset.Dir = wtPath
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("reset worktree to %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	clean := exec.Command("git", "clean", "-fd")
	clean.Dir = wtPath
	if out, err := clean.CombinedOutput(); err != nil {
		return fmt.Errorf("clean worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rebaseStateDir returns the path to the rebase-merge or rebase-apply dir.
func rebaseStateDir(wtPath string) string {
	// git worktrees store rebase state inside the .git dir (which is a file
	// pointing to the actual gitdir). Use rev-parse to resolve it.
	cmd := exec.Command("git", "rev-parse", "--git-dir")
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
func CreateWorktree(barePath, worktreePath, branch, baseBranch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "add", worktreePath, "-b", branch, baseBranch)
		})
	})
}

// CreateWorktreeExisting checks out an existing branch into a new worktree.
func CreateWorktreeExisting(barePath, worktreePath, branch string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "add", worktreePath, branch)
		})
	})
}

// BranchExists reports whether a local branch exists in the repo.
func BranchExists(barePath, branch string) bool {
	err := runBare(barePath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RefExists reports whether an arbitrary ref (e.g. refs/remotes/origin/<branch>)
// resolves in the repo.
func RefExists(barePath, ref string) bool {
	_, ok := resolveRef(barePath, ref)
	return ok
}

// CreateWorktreeDetached creates a worktree in detached HEAD mode from a remote ref.
// Used for read-only checkouts like code reviews.
func CreateWorktreeDetached(barePath, worktreePath, ref string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "add", "--detach", worktreePath, ref)
		})
	})
}

// ListWorktrees parses `git worktree list --porcelain` output for barePath
// into Worktree entries, excluding the bare repo's own record.
func ListWorktrees(barePath string) ([]Worktree, error) {
	out, err := outputBare(barePath, "worktree", "list", "--porcelain")
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
func PushRemote(repoPath string) string {
	if _, err := executil.Output(repoPath, "git", "config", "--get", "remote.fork.url"); err == nil {
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
func EnforceForkOnlyPush(worktreePath string) error {
	if _, err := executil.Output(worktreePath, "git", "config", "--get", "remote.fork.url"); err != nil {
		current, getErr := executil.Output(worktreePath, "git", "config", "--get", "remote.origin.pushurl")
		if getErr == nil && strings.TrimSpace(current) == forkOnlyDisabledPushURL {
			_ = executil.Run(worktreePath, "git", "config", "--unset", "remote.origin.pushurl")
		}
		return nil
	}
	return executil.Run(worktreePath, "git", "remote", "set-url", "--push", "origin", forkOnlyDisabledPushURL)
}

// PushUpstream pushes branch to the fork remote if present, else origin,
// with -u to set remote tracking.
func PushUpstream(worktreePath, branch string) error {
	return executil.Run(worktreePath, "git", "push", "-u", PushRemote(worktreePath), branch)
}

// CurrentBranch returns the checked-out branch name for a worktree.
func CurrentBranch(worktreePath string) (string, error) {
	branch, err := executil.Output(worktreePath, "git", "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(branch), nil
}

// isAncestor reports whether ancestor is reachable from descendant in the
// worktree's history. Returns false when either ref is unknown.
func isAncestor(worktreePath, ancestor, descendant string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = worktreePath
	return cmd.Run() == nil
}

// remoteBranchHead queries the live head SHA of branch on remote via ls-remote.
// Returns ("", nil) when the remote branch does not exist (never pushed).
func remoteBranchHead(worktreePath, remote, branch string) (string, error) {
	out, err := executil.Output(worktreePath, "git", "ls-remote", remote, "refs/heads/"+branch)
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
func ReconcileWithRemote(worktreePath, branch string) error {
	remote := PushRemote(worktreePath)
	// Refresh the tracking ref from the live remote; fork remotes are not
	// covered by the earlier FetchOrigin. A first-push branch has no remote
	// head yet, so "couldn't find remote ref" is expected and not fatal — any
	// other failure (network/auth/remote misconfig) must propagate, since
	// continuing on stale history is exactly the data-loss scenario this
	// function exists to prevent.
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	if err := executil.Run(worktreePath, "git", "fetch", remote, refspec); err != nil && !strings.Contains(err.Error(), "couldn't find remote ref") {
		return fmt.Errorf("fetch %s %s: %w", remote, refspec, err)
	}

	// An absent branch (first push) means there is nothing to reconcile; any
	// other ls-remote failure must propagate rather than silently no-op.
	remoteSHA, err := remoteBranchHead(worktreePath, remote, branch)
	if err != nil {
		return fmt.Errorf("ls-remote %s %s: %w", remote, branch, err)
	}
	if remoteSHA == "" {
		return nil
	}

	localSHA, err := executil.Output(worktreePath, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if localSHA == remoteSHA {
		return nil
	}
	if isAncestor(worktreePath, remoteSHA, localSHA) {
		return nil // local already contains the remote head
	}
	if isAncestor(worktreePath, localSHA, remoteSHA) {
		// Remote strictly ahead — adopt its commits before rebasing.
		return executil.Run(worktreePath, "git", "merge", "--ff-only", remoteSHA)
	}
	return fmt.Errorf("%w: local %s vs remote %s/%s %s", ErrBranchDiverged, localSHA[:min(7, len(localSHA))], remote, branch, remoteSHA[:min(7, len(remoteSHA))])
}

// PushSync syncs the branch to the fork remote (if present) or origin using
// the minimum mode required:
//   - first push (remote tracking ref absent): regular push with -u
//   - local SHA == remote tracking SHA: no-op
//   - remote tracking SHA is an ancestor of local (fast-forward): regular push
//   - histories diverged: --force-with-lease, but only after confirming the live
//     remote head still matches the tracking ref (else ErrRemoteAdvanced)
//
// Compared to an unconditional force push, this avoids gratuitous rewrites of
// the remote when a rebase was a no-op or the agent produced no new commits.
// Returns ErrBranchMissing if the local branch ref does not exist.
func PushSync(worktreePath, branch string) error {
	if err := executil.Run(worktreePath, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return ErrBranchMissing
		}
		return err
	}

	remote := PushRemote(worktreePath)
	localSHA, err := executil.Output(worktreePath, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return err
	}

	remoteSHA, remoteErr := executil.Output(worktreePath, "git", "rev-parse", "--verify", "refs/remotes/"+remote+"/"+branch)
	if remoteErr != nil {
		// Remote tracking ref unknown — first push, set upstream.
		return executil.Run(worktreePath, "git", "push", "-u", remote, branch)
	}

	if localSHA == remoteSHA {
		return nil
	}

	// Fast-forward when remote SHA is reachable from local SHA.
	if isAncestor(worktreePath, remoteSHA, localSHA) {
		return executil.Run(worktreePath, "git", "push", "-u", remote, branch)
	}

	// Divergence path: about to force-push. Verify the live remote head still
	// matches the tracking ref this decision was based on. If it advanced since
	// the last fetch, --force-with-lease would pass against the stale tracking
	// ref and clobber the newer commits — refuse instead. Fail closed if the
	// live head can't even be verified, rather than proceeding with a force
	// push against an unconfirmed remote state.
	liveSHA, err := remoteBranchHead(worktreePath, remote, branch)
	if err != nil {
		return fmt.Errorf("%w: could not verify live remote head before force push: %w", ErrRemoteAdvanced, err)
	}
	if liveSHA != "" && liveSHA != remoteSHA {
		return fmt.Errorf("%w: tracking %s but remote %s/%s is at %s", ErrRemoteAdvanced, remoteSHA[:min(7, len(remoteSHA))], remote, branch, liveSHA[:min(7, len(liveSHA))])
	}

	return executil.Run(worktreePath, "git", "push", "--force-with-lease", "-u", remote, branch)
}

// RemoveWorktree deletes worktreePath and its `git worktree` registration
// from barePath (`git worktree remove --force`), discarding any
// uncommitted changes in it. Use PruneWorktrees to clean up stale
// administrative entries left behind if the directory was already removed
// out-of-band.
func RemoveWorktree(barePath, worktreePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "remove", "--force", worktreePath)
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
func RemoveWorktreeReconcile(barePath, worktreePath string) error {
	// Registered case: this alone succeeds and removes the directory.
	_ = RemoveWorktree(barePath, worktreePath)
	// Drop stale admin entries so a subsequent `worktree add` doesn't trip
	// over a registration that no longer has a live directory either.
	_ = PruneWorktrees(barePath)
	_, statErr := os.Stat(worktreePath)
	switch {
	case statErr == nil:
		// Orphan case: the directory survived because git couldn't resolve
		// its admin entry. Fall back to a raw removal.
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("remove orphan worktree %s: %w", worktreePath, err)
		}
		_ = PruneWorktrees(barePath)
	case !os.IsNotExist(statErr):
		return fmt.Errorf("stat worktree %s: %w", worktreePath, statErr)
	}
	return nil
}

// PruneWorktrees removes stale worktree admin entries from the bare repo.
func PruneWorktrees(barePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "prune")
		})
	})
}

// WorktreeHealthy reports whether a checked-out worktree's git metadata is
// resolvable. A `false` result typically means the .git pointer references a
// path that no longer exists — the usual cause is a container redeploy that
// changed the in-container mount point of the bare clone.
func WorktreeHealthy(worktreePath string) bool {
	_, err := executil.Output(worktreePath, "git", "rev-parse", "--git-dir")
	return err == nil
}

// RepairWorktrees runs `git worktree repair` against the bare repo, which
// rewrites the absolute path back-pointers in every checked-out worktree's
// .git file and the bare's worktrees/<id>/gitdir file. Idempotent.
func RepairWorktrees(barePath string) error {
	return withBareRepoLock(barePath, func() error {
		return withLockRetry(func() error {
			return runBare(barePath, "worktree", "repair")
		})
	})
}

// RebaseOnto rebases the worktree's current branch onto the given ref.
// Aborts and returns an error on conflict.
func RebaseOnto(worktreePath, ref string) error {
	if err := executil.Run(worktreePath, "git", "rebase", ref); err != nil {
		_ = executil.Run(worktreePath, "git", "rebase", "--abort")
		return fmt.Errorf("rebase onto %s: %w", ref, err)
	}
	return nil
}
