// Package tasksnapshot versions the tasks dir into a separate git
// repository, giving recovery a `git checkout` path for external deleters
// that bypass task.Store's trash-based soft delete (the case the
// 2026-07-06 board wipe, #1576, needed audit-log forensics to reconstruct).
//
// A single Snapshotter polls the tasks dir on a fixed interval: `git add
// -A`, and if the index is dirty, commit. Polling — not event subscription
// — is the trigger, so a raw external `rm` is captured for free (`git add
// -A` is its own change detector). All git operations run against a
// dedicated GIT_DIR/GIT_WORK_TREE pair via direct exec.CommandContext argv
// (no shell, no remotes, no network) under a per-op timeout, and every
// error is logged and swallowed by the production entry points so task CRUD
// can never be broken by a snapshotting failure.
package tasksnapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// opTimeout bounds every individual git invocation.
const opTimeout = 15 * time.Second

// DefaultInterval is used when New is given a non-positive interval.
const DefaultInterval = 30 * time.Second

// Snapshotter owns a single git repository (at gitDir) versioning the
// contents of workTree on a fixed-interval commit loop. Safe for concurrent
// use; all git operations are serialized through mu so at most one commit
// runs at a time.
type Snapshotter struct {
	gitDir   string
	workTree string
	interval time.Duration
	logger   *slog.Logger

	mu         sync.Mutex
	disabled   bool
	lastCommit time.Time
}

// New constructs a Snapshotter. gitDir is the dedicated git-dir sibling
// (see config.TaskSnapshotGitDir); workTree is the tasks dir being
// versioned. A non-positive interval falls back to DefaultInterval, and a
// nil logger falls back to slog.Default().
func New(gitDir, workTree string, interval time.Duration, logger *slog.Logger) *Snapshotter {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Snapshotter{
		gitDir:   gitDir,
		workTree: workTree,
		interval: interval,
		logger:   logger,
	}
}

// BuildEnv returns the process environment with every inherited GIT_*
// variable stripped (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE,
// GIT_OBJECT_DIRECTORY, GIT_CEILING_DIRECTORIES, etc.) and only GIT_DIR/
// GIT_WORK_TREE re-applied, so every git invocation targets exactly this
// repo/work-tree regardless of the calling process's environment. A caller
// (e.g. a headless agent, or a parent process mid-rebase) that has any other
// GIT_* variable set — most notably GIT_INDEX_FILE — would otherwise make
// git read/write the wrong index file and silently fail to snapshot.
// Exported so read-only callers outside the package (e.g. sybra-cli's
// tasks-history) target the same repo the same way.
func BuildEnv(gitDir, workTree string) []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+2)
	for _, e := range base {
		if strings.HasPrefix(e, "GIT_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "GIT_DIR="+gitDir, "GIT_WORK_TREE="+workTree)
}

// run executes `git <args...>` against this Snapshotter's git-dir/work-tree,
// bounded by opTimeout, and returns trimmed stdout. Never touches remotes —
// callers must not pass fetch/push/pull/clone.
func (s *Snapshotter) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = BuildEnv(s.gitDir, s.workTree)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// hasStagedChanges reports whether the index differs from HEAD, using the
// exit code of `git diff --cached --quiet` (0 = clean, 1 = dirty) rather
// than treating any non-zero exit as an error.
func (s *Snapshotter) hasStagedChanges(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Env = BuildEnv(s.gitDir, s.workTree)
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// EnsureRepo initializes the git repo at s.gitDir if absent, or validates a
// pre-existing one before ever writing to it. Returns whether the
// snapshotter is usable — false disables all future Commit/CommitNow calls
// for the lifetime of this Snapshotter (git missing, an unresolvable/corrupt
// repo, or an existing repo whose work-tree does not match s.workTree).
func (s *Snapshotter) EnsureRepo(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return false
	}

	if _, err := exec.LookPath("git"); err != nil {
		s.logger.Warn("tasksnapshot.git_missing", "err", err)
		s.disabled = true
		return false
	}
	if err := os.MkdirAll(filepath.Dir(s.gitDir), 0o755); err != nil {
		s.logger.Warn("tasksnapshot.mkdir_failed", "dir", filepath.Dir(s.gitDir), "err", err)
		s.disabled = true
		return false
	}
	if err := os.MkdirAll(s.workTree, 0o755); err != nil {
		s.logger.Warn("tasksnapshot.worktree_mkdir_failed", "dir", s.workTree, "err", err)
		s.disabled = true
		return false
	}

	info, statErr := os.Stat(s.gitDir)
	switch {
	case statErr == nil && !info.IsDir():
		s.logger.Warn("tasksnapshot.gitdir_not_a_directory", "path", s.gitDir)
		s.disabled = true
		return false
	case statErr != nil && !os.IsNotExist(statErr):
		s.logger.Warn("tasksnapshot.stat_failed", "path", s.gitDir, "err", statErr)
		s.disabled = true
		return false
	case statErr != nil:
		if err := s.initRepo(ctx); err != nil {
			s.logger.Warn("tasksnapshot.init_failed", "err", err)
			s.disabled = true
			return false
		}
		return true
	default:
		if err := s.validateExisting(ctx); err != nil {
			s.logger.Warn("tasksnapshot.validate_failed", "err", err)
			s.disabled = true
			return false
		}
		return true
	}
}

// initRepo creates a fresh repo at s.gitDir with a work-tree at s.workTree,
// a bot identity (so commits never depend on ambient git config), gc
// disabled (history is kept in full — see D2 in the plan, pruning deferred),
// and *.lock excluded so a stray lock file never gets committed.
func (s *Snapshotter) initRepo(ctx context.Context) error {
	if _, err := s.run(ctx, "init"); err != nil {
		return err
	}
	if _, err := s.run(ctx, "config", "user.name", "Sybra Snapshotter"); err != nil {
		return err
	}
	if _, err := s.run(ctx, "config", "user.email", "sybra-snapshotter@localhost"); err != nil {
		return err
	}
	if _, err := s.run(ctx, "config", "gc.auto", "0"); err != nil {
		return err
	}
	if _, err := s.run(ctx, "config", "core.worktree", s.workTree); err != nil {
		return err
	}
	return s.ensureLockExcluded()
}

// validateExisting confirms a pre-existing s.gitDir is a resolvable git
// repo whose recorded work-tree matches s.workTree, so this Snapshotter
// never writes into a repo that was previously versioning a different
// directory (e.g. gitDir reused across a config change).
func (s *Snapshotter) validateExisting(ctx context.Context) error {
	if _, err := s.run(ctx, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a valid git repo: %w", err)
	}
	wt, err := s.run(ctx, "config", "--get", "core.worktree")
	if err != nil {
		return fmt.Errorf("read core.worktree: %w", err)
	}
	wantAbs, errWant := filepath.Abs(s.workTree)
	gotAbs, errGot := filepath.Abs(wt)
	if errWant != nil || errGot != nil || wantAbs != gotAbs {
		return fmt.Errorf("existing repo work-tree %q does not match expected %q", wt, s.workTree)
	}
	return s.ensureLockExcluded()
}

// ensureLockExcluded appends "*.lock" to $GIT_DIR/info/exclude if it is not
// already present. Idempotent, safe to call on every EnsureRepo.
func (s *Snapshotter) ensureLockExcluded() error {
	excludePath := filepath.Join(s.gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir info dir: %w", err)
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read exclude file: %w", err)
	}
	if slices.Contains(strings.Split(string(existing), "\n"), "*.lock") {
		return nil
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude file: %w", err)
	}
	defer f.Close()
	pattern := "*.lock\n"
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		pattern = "\n" + pattern
	}
	if _, err := f.WriteString(pattern); err != nil {
		return fmt.Errorf("write exclude file: %w", err)
	}
	return nil
}

// Commit stages and, if the index is dirty, commits every change under the
// work-tree (including files removed by an external `rm` — `git add -A` is
// its own change detector). Returns whether a commit was made. A no-op
// (false, nil) when EnsureRepo has never succeeded or disabled the
// snapshotter, or when the tree is already clean.
func (s *Snapshotter) Commit(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(ctx)
}

func (s *Snapshotter) commitLocked(ctx context.Context) (bool, error) {
	if s.disabled {
		return false, nil
	}
	if _, err := s.run(ctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %w", err)
	}
	dirty, err := s.hasStagedChanges(ctx)
	if err != nil {
		return false, fmt.Errorf("git diff: %w", err)
	}
	if !dirty {
		return false, nil
	}
	msg := fmt.Sprintf("snapshot: %s", time.Now().UTC().Format(time.RFC3339))
	if _, err := s.run(ctx, "commit", "-m", msg); err != nil {
		return false, fmt.Errorf("git commit: %w", err)
	}
	s.lastCommit = time.Now()
	return true, nil
}

// CommitNow is the nil-safe, error-swallowing entry point used by
// production call sites (Run's ticker, recovery.Recovery.CommitBeforePrune)
// where a git failure must never break task CRUD or the trash-prune sweep.
// Matches the func(context.Context) shape recovery.Recovery.CommitBeforePrune
// expects, so it can be assigned directly as a method value.
func (s *Snapshotter) CommitNow(ctx context.Context) {
	if s == nil {
		return
	}
	if _, err := s.Commit(ctx); err != nil {
		s.logger.Warn("tasksnapshot.commit_failed", "err", err)
	}
}

// Run polls on a fixed interval — not an idle debounce — calling CommitNow
// until ctx is done. At most one commit runs per interval; worst-case data
// loss on an unclean shutdown is bounded by one interval. Callers are
// expected to have called EnsureRepo (and ideally taken a baseline commit)
// before launching Run.
func (s *Snapshotter) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CommitNow(ctx)
		}
	}
}

// LastCommit returns the time of the last successful commit, or the zero
// time if none has happened yet.
func (s *Snapshotter) LastCommit() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCommit
}
