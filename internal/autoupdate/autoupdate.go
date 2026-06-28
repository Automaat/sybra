package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	ModeAuto        = "auto"
	ModeNotify      = "notify"
	RestartExitCode = 42
)

type Config struct {
	Enabled        bool
	RepoDir        string
	Remote         string
	Branch         string
	Mode           string
	PollInterval   time.Duration
	RequestRestart func()
}

type Result struct {
	Status       string
	Reason       string
	OldSHA       string
	NewSHA       string
	ChangedFiles []string
}

type Runner struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{cfg: cfg.withDefaults(), logger: logger}
}

func (r *Runner) Run(ctx context.Context) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	r.logger.Info("autoupdate.enabled",
		"repo", r.cfg.RepoDir,
		"remote", r.cfg.Remote,
		"branch", r.cfg.Branch,
		"mode", r.cfg.Mode,
		"poll", r.cfg.PollInterval)

	r.check(ctx)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.check(ctx)
		}
	}
}

func (r *Runner) CheckAndApply(ctx context.Context) (Result, error) {
	cfg := r.cfg.withDefaults()
	if !cfg.Enabled {
		return Result{Status: "disabled"}, nil
	}
	if cfg.Mode != ModeAuto && cfg.Mode != ModeNotify {
		return Result{Status: "blocked", Reason: "invalid mode: " + cfg.Mode}, nil
	}

	if reason := repoBlockReason(ctx, cfg); reason != "" {
		return Result{Status: "blocked", Reason: reason}, nil
	}
	if _, err := git(ctx, cfg.RepoDir, "fetch", "--quiet", cfg.Remote,
		fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", cfg.Branch, cfg.Remote, cfg.Branch)); err != nil {
		return Result{}, fmt.Errorf("fetch %s/%s: %w", cfg.Remote, cfg.Branch, err)
	}

	head, err := git(ctx, cfg.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	remoteRef := fmt.Sprintf("refs/remotes/%s/%s", cfg.Remote, cfg.Branch)
	remoteSHA, err := git(ctx, cfg.RepoDir, "rev-parse", remoteRef)
	if err != nil {
		return Result{}, fmt.Errorf("rev-parse %s: %w", remoteRef, err)
	}
	if head == remoteSHA {
		return Result{Status: "current", OldSHA: head, NewSHA: remoteSHA}, nil
	}
	if !isAncestor(ctx, cfg.RepoDir, head, remoteSHA) {
		return Result{Status: "blocked", Reason: "local branch is ahead or diverged", OldSHA: head, NewSHA: remoteSHA}, nil
	}

	changed, err := changedFiles(ctx, cfg.RepoDir, remoteSHA)
	if err != nil {
		return Result{}, err
	}
	res := Result{Status: "available", OldSHA: head, NewSHA: remoteSHA, ChangedFiles: changed}
	if cfg.Mode != ModeAuto {
		return res, nil
	}
	if _, err := git(ctx, cfg.RepoDir, "merge", "--ff-only", remoteRef); err != nil {
		return Result{}, fmt.Errorf("fast-forward %s: %w", remoteRef, err)
	}
	res.Status = "applied"
	return res, nil
}

func (r *Runner) check(ctx context.Context) {
	res, err := r.CheckAndApply(ctx)
	if err != nil {
		r.logger.Error("autoupdate.check.failed", "err", err)
		return
	}
	attrs := []any{"status", res.Status}
	if res.Reason != "" {
		attrs = append(attrs, "reason", res.Reason)
	}
	if res.OldSHA != "" && res.NewSHA != "" {
		attrs = append(attrs, "old", shortSHA(res.OldSHA), "new", shortSHA(res.NewSHA))
	}
	if len(res.ChangedFiles) > 0 {
		attrs = append(attrs, "changed", res.ChangedFiles)
	}
	r.logger.Info("autoupdate.check", attrs...)
	if res.Status == "applied" && r.cfg.RequestRestart != nil {
		r.logger.Info("autoupdate.restart.requested")
		r.cfg.RequestRestart()
	}
}

func validateRepoState(ctx context.Context, cfg Config) error {
	if cfg.RepoDir == "" {
		return errors.New("repo_dir is empty")
	}
	if _, err := git(ctx, cfg.RepoDir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return fmt.Errorf("not a git worktree: %w", err)
	}
	branch, err := git(ctx, cfg.RepoDir, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("current branch: %w", err)
	}
	if branch != cfg.Branch {
		return fmt.Errorf("wrong branch %q, want %q", branch, cfg.Branch)
	}
	if err := ensureNoGitOperation(ctx, cfg.RepoDir); err != nil {
		return err
	}
	status, err := git(ctx, cfg.RepoDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("worktree is dirty")
	}
	return nil
}

func repoBlockReason(ctx context.Context, cfg Config) string {
	if err := validateRepoState(ctx, cfg); err != nil {
		return err.Error()
	}
	return ""
}

func isAncestor(ctx context.Context, repoDir, ancestor, descendant string) bool {
	return gitRun(ctx, repoDir, "merge-base", "--is-ancestor", ancestor, descendant) == nil
}

func ensureNoGitOperation(ctx context.Context, repoDir string) error {
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		p, err := git(ctx, repoDir, "rev-parse", "--git-path", name)
		if err != nil {
			return fmt.Errorf("git-path %s: %w", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(repoDir, p)); statErr == nil {
			return fmt.Errorf("git operation in progress: %s", name)
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("check git operation %s: %w", name, statErr)
		}
	}
	return nil
}

func changedFiles(ctx context.Context, repoDir, remoteSHA string) ([]string, error) {
	out, err := git(ctx, repoDir, "diff", "--name-only", "HEAD", remoteSHA)
	if err != nil {
		return nil, fmt.Errorf("diff changed files: %w", err)
	}
	lines := strings.Split(out, "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func gitRun(ctx context.Context, dir, name string, args ...string) error {
	_, err := git(ctx, dir, name, args...)
	return err
}

func git(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{name}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (c Config) withDefaults() Config {
	if c.Remote == "" {
		c.Remote = "origin"
	}
	if c.Branch == "" {
		c.Branch = "main"
	}
	if c.Mode == "" {
		c.Mode = ModeNotify
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Minute
	}
	return c
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
