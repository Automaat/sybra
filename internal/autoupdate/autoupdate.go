package autoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
)

const (
	ModeAuto        = "auto"
	ModeNotify      = "notify"
	RestartMarker   = "restart-requested"
	RestartExitCode = 42
)

type Config struct {
	Enabled         bool
	RepoDir         string
	Remote          string
	Branch          string
	Mode            string
	Repository      string
	RequiredChecks  []string
	PollInterval    time.Duration
	StateFile       string
	OverrideFile    string
	RequestRestart  func()
	AuditTransition func(map[string]any)
	Now             func() time.Time
	GateCommit      func(context.Context, string, string, []string) (github.CommitGate, error)
}

type Result struct {
	Status       string
	Reason       string
	Repo         string
	OldSHA       string
	NewSHA       string
	ChangedFiles []string
}

type Runner struct {
	cfg      Config
	logger   *slog.Logger
	checkNow chan struct{}
}

type persistedState struct {
	CandidateSHA    string    `json:"candidate_sha,omitempty"`
	CandidateState  string    `json:"candidate_state,omitempty"`
	CandidateReason string    `json:"candidate_reason,omitempty"`
	CandidateSeenAt time.Time `json:"candidate_seen_at,omitzero"`
	PendingSHA      string    `json:"pending_sha,omitempty"`
	PendingAt       time.Time `json:"pending_at,omitzero"`
	LastAppliedSHA  string    `json:"last_applied_sha,omitempty"`
	LastAppliedAt   time.Time `json:"last_applied_at,omitzero"`
	LastRestartAt   time.Time `json:"last_restart_at,omitzero"`
}

func New(cfg Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:      cfg.withDefaults(),
		logger:   logger,
		checkNow: make(chan struct{}, 1),
	}
}

func RestartMarkerPath(homeDir string) string {
	return filepath.Join(homeDir, RestartMarker)
}

func WriteRestartMarker(homeDir string) error {
	if homeDir == "" {
		return errors.New("home dir is empty")
	}
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return fmt.Errorf("create sybra home: %w", err)
	}
	if err := os.WriteFile(RestartMarkerPath(homeDir), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write restart marker: %w", err)
	}
	return nil
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
		case <-r.checkNow:
			r.check(ctx)
		case <-ticker.C:
			r.check(ctx)
		}
	}
}

// TriggerCheck coalesces an on-demand autoupdate check into the run loop.
func (r *Runner) TriggerCheck() {
	if r == nil || !r.cfg.Enabled {
		return
	}
	select {
	case r.checkNow <- struct{}{}:
	default:
	}
}

func (r *Runner) CheckAndApply(ctx context.Context) (Result, error) {
	cfg := r.cfg.withDefaults()
	if result, ok := preflightResult(ctx, cfg); ok {
		return result, nil
	}
	head, remoteSHA, err := resolveSHAs(ctx, cfg)
	if err != nil {
		return Result{}, err
	}
	if head == remoteSHA {
		r.clearPendingState(cfg.StateFile, head)
		return Result{Status: "current", OldSHA: head, NewSHA: remoteSHA}, nil
	}
	if result, blocked, err := validateCandidateSHA(ctx, cfg, head, remoteSHA); err != nil || blocked {
		if err != nil {
			return Result{}, err
		}
		return result, nil
	}
	changed, err := changedFiles(ctx, cfg.RepoDir, head, remoteSHA)
	if err != nil {
		return Result{}, err
	}
	state, err := loadState(cfg.StateFile)
	if err != nil {
		return Result{}, err
	}
	now := cfg.now()
	r.noteCandidateSeen(&state, head, remoteSHA, now)

	overrideActive, err := overrideRequested(cfg.OverrideFile)
	if err != nil {
		return Result{}, err
	}
	repo, gateResult, err := r.evaluateCandidate(ctx, cfg, &state, remoteSHA, head, changed, now, overrideActive)
	if err != nil {
		return Result{}, err
	}
	if err := saveCandidateState(cfg.StateFile, state, gateResult != nil || cfg.Mode != ModeAuto || state.PendingSHA == remoteSHA); err != nil {
		return Result{}, err
	}
	if gateResult != nil {
		return *gateResult, nil
	}
	if cfg.Mode != ModeAuto {
		return Result{Status: "approved", Reason: state.CandidateReason, Repo: repo, OldSHA: head, NewSHA: remoteSHA, ChangedFiles: changed}, nil
	}
	if result, waiting, err := r.handleSupersededCandidate(ctx, cfg, &state, head, remoteSHA, repo, now); err != nil || waiting {
		if err != nil {
			return Result{}, err
		}
		return result, nil
	}
	if _, err := fetchObject(ctx, cfg.RepoDir, cfg.Remote, remoteSHA); err != nil {
		return Result{}, err
	}
	if overrideActive {
		if err := clearOverride(cfg.OverrideFile); err != nil {
			return Result{}, err
		}
	}
	if _, err := git(ctx, cfg.RepoDir, "merge", "--ff-only", remoteSHA); err != nil {
		return Result{}, fmt.Errorf("fast-forward %s: %w", remoteSHA, err)
	}
	state.PendingSHA = ""
	state.PendingAt = time.Time{}
	state.LastAppliedSHA = remoteSHA
	state.LastAppliedAt = now
	state.LastRestartAt = now
	state.setCandidateOutcome("applied", "")
	r.recordTransition("applied", remoteSHA, head, remoteSHA, "")
	reason := ""
	if err := saveState(cfg.StateFile, state); err != nil {
		reason = "post-apply state update failed: " + err.Error()
		r.logger.Warn("autoupdate.post_apply_state.failed", "err", err, "sha", shortSHA(remoteSHA))
	}
	return Result{Status: "applied", Reason: reason, Repo: repo, OldSHA: head, NewSHA: remoteSHA, ChangedFiles: changed}, nil
}

func (r *Runner) noteCandidateSeen(state *persistedState, head, remoteSHA string, now time.Time) {
	if state.CandidateSHA == remoteSHA {
		return
	}
	if state.CandidateSHA != "" && state.CandidateSHA != head {
		r.recordTransition("superseded", state.CandidateSHA, head, remoteSHA, "candidate replaced by newer remote sha")
	}
	state.CandidateSHA = remoteSHA
	state.CandidateState = "seen"
	state.CandidateReason = ""
	state.CandidateSeenAt = now
	r.recordTransition("seen", remoteSHA, head, remoteSHA, "")
}

func preflightResult(ctx context.Context, cfg Config) (Result, bool) {
	if !cfg.Enabled {
		return Result{Status: "disabled"}, true
	}
	if cfg.Mode != ModeAuto && cfg.Mode != ModeNotify {
		return Result{Status: "blocked", Reason: "invalid mode: " + cfg.Mode}, true
	}
	if reason := repoBlockReason(ctx, cfg); reason != "" {
		return Result{Status: "blocked", Reason: reason}, true
	}
	return Result{}, false
}

func resolveSHAs(ctx context.Context, cfg Config) (head, remoteSHA string, err error) {
	head, err = git(ctx, cfg.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	remoteSHA, err = lsRemoteBranchSHA(ctx, cfg.RepoDir, cfg.Remote, cfg.Branch)
	if err != nil {
		return "", "", fmt.Errorf("resolve remote sha %s/%s: %w", cfg.Remote, cfg.Branch, err)
	}
	return head, remoteSHA, nil
}

func saveCandidateState(path string, state persistedState, shouldSave bool) error {
	if !shouldSave {
		return nil
	}
	return saveState(path, state)
}

func clearPendingState(path, head string) error {
	if path == "" {
		return nil
	}
	state, err := loadState(path)
	if err != nil {
		return err
	}
	if state.PendingSHA != head {
		return nil
	}
	state.PendingSHA = ""
	state.PendingAt = time.Time{}
	return saveState(path, state)
}

func (r *Runner) clearPendingState(path, head string) {
	if err := clearPendingState(path, head); err != nil {
		r.logger.Warn("autoupdate.pending_state_clear.failed", "err", err, "sha", shortSHA(head))
	}
}

func validateCandidateSHA(ctx context.Context, cfg Config, head, remoteSHA string) (Result, bool, error) {
	if _, err := fetchObject(ctx, cfg.RepoDir, cfg.Remote, remoteSHA); err != nil {
		return Result{}, false, err
	}
	if isAncestor(ctx, cfg.RepoDir, head, remoteSHA) {
		return Result{}, false, nil
	}
	return Result{Status: "blocked", Reason: "local branch is ahead or diverged", OldSHA: head, NewSHA: remoteSHA}, true, nil
}

func (r *Runner) handleSupersededCandidate(ctx context.Context, cfg Config, state *persistedState, head, remoteSHA, repo string, now time.Time) (Result, bool, error) {
	latestSHA, err := lsRemoteBranchSHA(ctx, cfg.RepoDir, cfg.Remote, cfg.Branch)
	if err != nil {
		return Result{}, false, fmt.Errorf("re-resolve remote sha %s/%s: %w", cfg.Remote, cfg.Branch, err)
	}
	if latestSHA == remoteSHA {
		return Result{}, false, nil
	}
	r.recordTransition("superseded", remoteSHA, head, latestSHA, "candidate changed before apply")
	state.CandidateSHA = latestSHA
	state.CandidateState = "seen"
	state.CandidateReason = ""
	state.CandidateSeenAt = now
	state.PendingSHA = ""
	state.PendingAt = time.Time{}
	if err := saveState(cfg.StateFile, *state); err != nil {
		return Result{}, false, err
	}
	return Result{Status: "waiting", Reason: "candidate changed before apply", Repo: repo, OldSHA: head, NewSHA: latestSHA}, true, nil
}

func (r *Runner) evaluateCandidate(ctx context.Context, cfg Config, state *persistedState, remoteSHA, head string, changed []string, now time.Time, overrideActive bool) (string, *Result, error) {
	if overrideActive {
		repo, _ := cfg.repository(ctx)
		state.PendingSHA = remoteSHA
		state.PendingAt = now
		state.setCandidateOutcome("approved", "manual override")
		r.recordTransition("approved", remoteSHA, head, remoteSHA, "manual override")
		return repo, nil, nil
	}
	required := github.NormalizeRequiredChecks(cfg.RequiredChecks)
	if len(required) == 0 {
		return "", r.rejectEmptyRequiredChecks(cfg, state, remoteSHA, head, changed), nil
	}
	repo, err := cfg.repository(ctx)
	if err != nil {
		return "", nil, err
	}
	if result := r.ensureGithubToken(ctx, cfg, state, repo, remoteSHA, head, changed); result != nil {
		return repo, result, nil
	}
	gate, err := cfg.gateCommit(ctx, repo, remoteSHA, required)
	if err != nil {
		return repo, r.rejectCandidate(state, repo, remoteSHA, head, changed, err.Error()), nil
	}
	if reason, waiting := gateBlockReason(gate); reason != "" {
		status := "rejected"
		outcome := "rejected"
		if waiting {
			status = "waiting"
			outcome = "waiting"
		}
		return repo, r.rejectCandidateWithStatus(state, repo, remoteSHA, head, changed, outcome, status, reason), nil
	}
	state.PendingSHA = remoteSHA
	state.PendingAt = now
	state.setCandidateOutcome("approved", "")
	r.recordTransition("approved", remoteSHA, head, remoteSHA, strings.Join(required, ","))
	return repo, nil, nil
}

func (r *Runner) rejectEmptyRequiredChecks(cfg Config, state *persistedState, remoteSHA, head string, changed []string) *Result {
	reason := "required checks are empty"
	if cfg.Mode != ModeAuto {
		state.setCandidateOutcome("seen", reason)
		return &Result{Status: "available", Reason: reason, OldSHA: head, NewSHA: remoteSHA, ChangedFiles: changed}
	}
	return r.rejectCandidate(state, "", remoteSHA, head, changed, reason)
}

func (r *Runner) ensureGithubToken(ctx context.Context, cfg Config, state *persistedState, repo, remoteSHA, head string, changed []string) *Result {
	if cfg.GateCommit != nil {
		return nil
	}
	if err := github.RefreshAppToken(ctx); err != nil {
		return r.rejectCandidate(state, repo, remoteSHA, head, changed, "github app token refresh failed: "+err.Error())
	}
	if github.AppAuthEnabled() && github.CurrentAppToken() == "" {
		return r.rejectCandidate(state, repo, remoteSHA, head, changed, "github app token unavailable")
	}
	return nil
}

func (r *Runner) rejectCandidate(state *persistedState, repo, remoteSHA, head string, changed []string, reason string) *Result {
	return r.rejectCandidateWithStatus(state, repo, remoteSHA, head, changed, "rejected", "rejected", reason)
}

func (r *Runner) rejectCandidateWithStatus(state *persistedState, repo, remoteSHA, head string, changed []string, outcome, status, reason string) *Result {
	state.setCandidateOutcome(outcome, reason)
	r.recordTransition(outcome, remoteSHA, head, remoteSHA, reason)
	return &Result{Status: status, Reason: reason, Repo: repo, OldSHA: head, NewSHA: remoteSHA, ChangedFiles: changed}
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
	if res.Repo != "" {
		attrs = append(attrs, "repo", res.Repo)
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

func changedFiles(ctx context.Context, repoDir, fromSHA, toSHA string) ([]string, error) {
	out, err := git(ctx, repoDir, "diff", "--name-only", fromSHA, toSHA)
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

func lsRemoteBranchSHA(ctx context.Context, repoDir, remote, branch string) (string, error) {
	out, err := git(ctx, repoDir, "ls-remote", "--exit-code", "--heads", remote, branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] == "" {
		return "", errors.New("remote branch sha not found")
	}
	return fields[0], nil
}

func fetchObject(ctx context.Context, repoDir, remote, sha string) (string, error) {
	if _, err := git(ctx, repoDir, "fetch", "--quiet", remote, sha); err != nil {
		return "", fmt.Errorf("fetch %s: %w", sha, err)
	}
	return sha, nil
}

func remoteRepo(ctx context.Context, repoDir, remote string) (string, error) {
	url, err := git(ctx, repoDir, "remote", "get-url", remote)
	if err != nil {
		return "", fmt.Errorf("remote %s url: %w", remote, err)
	}
	repo := strings.TrimSpace(url)
	repo = strings.TrimSuffix(repo, ".git")
	switch {
	case strings.HasPrefix(repo, "https://github.com/"):
		return strings.TrimPrefix(repo, "https://github.com/"), nil
	case strings.HasPrefix(repo, "http://github.com/"):
		return strings.TrimPrefix(repo, "http://github.com/"), nil
	case strings.HasPrefix(repo, "git@github.com:"):
		return strings.TrimPrefix(repo, "git@github.com:"), nil
	case strings.HasPrefix(repo, "ssh://git@github.com/"):
		return strings.TrimPrefix(repo, "ssh://git@github.com/"), nil
	default:
		return "", fmt.Errorf("unsupported github remote url %q", url)
	}
}

func gateBlockReason(gate github.CommitGate) (reason string, waiting bool) {
	switch {
	case len(gate.Missing) > 0:
		return "missing required checks: " + strings.Join(gate.Missing, ", "), false
	case len(gate.Failed) > 0:
		return "failed required checks: " + strings.Join(gate.Failed, ", "), false
	case len(gate.Pending) > 0:
		return "pending required checks: " + strings.Join(gate.Pending, ", "), true
	default:
		return "", false
	}
}

func loadState(path string) (persistedState, error) {
	if path == "" {
		return persistedState{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return persistedState{}, nil
		}
		return persistedState{}, fmt.Errorf("read autoupdate state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("parse autoupdate state: %w", err)
	}
	return state, nil
}

func saveState(path string, state persistedState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create autoupdate state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal autoupdate state: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write autoupdate state: %w", err)
	}
	return nil
}

func overrideRequested(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat autoupdate override: %w", err)
}

func clearOverride(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear autoupdate override: %w", err)
	}
	return nil
}

func (s *persistedState) setCandidateOutcome(state, reason string) {
	if s == nil {
		return
	}
	s.CandidateState = state
	s.CandidateReason = reason
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
	c.RequiredChecks = github.NormalizeRequiredChecks(c.RequiredChecks)
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Config) gateCommit(ctx context.Context, repo, sha string, required []string) (github.CommitGate, error) {
	if c.GateCommit != nil {
		return c.GateCommit(ctx, repo, sha, required)
	}
	return github.FetchCommitGate(ctx, repo, sha, required)
}

func (c Config) repository(ctx context.Context) (string, error) {
	if strings.TrimSpace(c.Repository) != "" {
		return strings.TrimSpace(c.Repository), nil
	}
	return remoteRepo(ctx, c.RepoDir, c.Remote)
}

func (r *Runner) recordTransition(transition, candidateSHA, oldSHA, newSHA, reason string) {
	if r == nil || r.cfg.AuditTransition == nil {
		return
	}
	data := map[string]any{
		"transition": transition,
		"candidate":  candidateSHA,
	}
	if oldSHA != "" {
		data["old_sha"] = oldSHA
	}
	if newSHA != "" {
		data["new_sha"] = newSHA
	}
	if reason != "" {
		data["reason"] = reason
	}
	r.cfg.AuditTransition(data)
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
