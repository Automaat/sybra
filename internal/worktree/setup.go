package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

// runSetup executes a project's setup commands inside the worktree directory.
// Every command runs via `sh -c` in wtPath with a shared batch timeout. All
// stdout/stderr is streamed to a per-task log file so agents (and operators)
// can inspect bootstrap failures without digging through the global log.
//
// Returns an error on the first non-zero exit or on timeout. Callers must
// treat setup failure as blocking: an agent started on a worktree with a
// broken toolchain will burn tokens hitting missing-tool errors.
func (m *Manager) runSetup(parent context.Context, taskID, wtPath string, commands []string) error {
	if len(commands) == 0 {
		return nil
	}

	logPath := m.setupLogPath(taskID)
	var logFile *os.File
	if logPath != "" {
		f, logErr := m.openSetupLog(logPath)
		if logErr != nil {
			// Missing log dir is not fatal — fall back to slog only. Agents can
			// still run; operators debug via sybra.log.
			m.logger.Warn("worktree.setup-log-open", "task_id", taskID, "path", logPath, "err", logErr)
		} else {
			logFile = f
		}
	}
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	writeLog := func(s string) {
		if logFile == nil {
			return
		}
		_, _ = logFile.WriteString(s)
	}

	ctx, cancel := context.WithTimeout(parent, m.setupTimeout)
	defer cancel()

	writeLog(fmt.Sprintf(
		"=== worktree setup: task=%s path=%s started_at=%s timeout=%s commands=%d ===\n",
		taskID, wtPath, time.Now().UTC().Format(time.RFC3339), m.setupTimeout, len(commands),
	))

	// mise refuses to read an untrusted config, so a fresh worktree whose
	// mise.toml has never been seen on this machine hard-fails `mise install`
	// with "Config files ... are not trusted". Trust is persisted per-path in
	// mise's state dir, so one call per worktree is enough and it is cheap
	// when the file is already trusted. Skipped when the worktree has no
	// mise config; failure is logged but non-fatal (the real setup command
	// will raise a clearer error if mise is actually needed).
	if hasMiseConfig(wtPath) {
		trustCmd := exec.CommandContext(ctx, m.misePath, "trust", "--yes")
		trustCmd.Dir = wtPath
		setProcessGroupKill(trustCmd)
		if logFile != nil {
			trustCmd.Stdout = logFile
			trustCmd.Stderr = logFile
		}
		writeLog(fmt.Sprintf("\n--- [pre] %s\n$ mise trust --yes\n", time.Now().UTC().Format(time.RFC3339)))
		if trustErr := trustCmd.Run(); trustErr != nil {
			writeLog(fmt.Sprintf("\n!!! mise trust exit err=%v (non-fatal)\n", trustErr))
			m.logger.Warn("worktree.mise-trust",
				"task_id", taskID, "path", wtPath, "err", trustErr)
		} else {
			writeLog("\n<<< ok\n")
		}
	}

	for i, raw := range commands {
		if err := ctx.Err(); err != nil {
			writeLog(fmt.Sprintf("\n!!! timeout before command %d (%s): %v\n", i+1, raw, err))
			m.logger.Error("worktree.setup-timeout",
				"task_id", taskID, "path", wtPath, "cmd", raw, "err", err,
				"log", logPath)
			return fmt.Errorf("setup timeout before command %q: %w", raw, err)
		}

		started := time.Now()
		writeLog(fmt.Sprintf("\n--- [%d/%d] %s\n$ %s\n", i+1, len(commands), started.UTC().Format(time.RFC3339), raw))

		cmd := exec.CommandContext(ctx, "sh", "-c", raw)
		cmd.Dir = wtPath
		setProcessGroupKill(cmd)
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}

		m.logger.Info("worktree.setup-start",
			"task_id", taskID, "path", wtPath, "cmd", raw, "index", i+1, "total", len(commands))

		err := cmd.Run()
		dur := time.Since(started)

		if err != nil {
			writeLog(fmt.Sprintf("\n!!! exit err=%v duration=%s\n", err, dur))
			m.logger.Error("worktree.setup-fail",
				"task_id", taskID, "path", wtPath, "cmd", raw,
				"index", i+1, "total", len(commands),
				"duration", dur, "err", err, "log", logPath)
			return fmt.Errorf("setup command %q failed after %s: %w (see %s)", raw, dur, err, logPath)
		}

		writeLog(fmt.Sprintf("\n<<< ok duration=%s\n", dur))
		m.logger.Info("worktree.setup-ok",
			"task_id", taskID, "path", wtPath, "cmd", raw,
			"index", i+1, "total", len(commands), "duration", dur)
	}

	writeLog(fmt.Sprintf("\n=== worktree setup: task=%s completed_at=%s ===\n",
		taskID, time.Now().UTC().Format(time.RFC3339)))
	m.logger.Info("worktree.setup-complete",
		"task_id", taskID, "path", wtPath, "commands", len(commands), "log", logPath)
	return nil
}

// setProcessGroupKill puts cmd in its own process group and wires its
// context-cancel to kill the whole group, not just the direct child. Setup
// commands run via `sh -c` and frequently fork further children (npm
// install, mise-managed toolchain daemons); the default exec.CommandContext
// cancel behavior SIGKILLs only the shell, leaking grandchildren once the
// batch timeout fires (issue #1538).
func setProcessGroupKill(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole process group (valid because
		// Setpgid made this process its own group leader, so pgid == pid).
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return err
		}
		return nil
	}
}

// runSetupNonGating runs a worktree's setup commands like runSetup, but never
// aborts worktree creation on failure. Fix-role worktrees (PrepareForFix,
// PrepareForBranchFix) exist specifically to repair a broken build/CI run, so
// gating their own creation on that exact breakage is a deadlock: the task can
// never dispatch an agent to fix what blocks it from starting (issue #1454).
// A failure is instead captured into the worktree's NOTES.md scratchpad,
// which SeedWorkingMemory already inlines into fix-role agent prompts, so the
// fixer sees the setup failure as its starting signal instead of the task
// silently stranding in todo.
func (m *Manager) runSetupNonGating(ctx context.Context, taskID, wtPath string, commands []string) error {
	setupErr := m.runSetup(ctx, taskID, wtPath, commands)
	if setupErr == nil {
		if err := clearSetupFailureMarker(wtPath); err != nil {
			m.logger.Warn("worktree.setup-fail-marker-clear", "task_id", taskID, "path", wtPath, "err", err)
		}
		return nil
	}
	m.logger.Warn("worktree.setup-fail-nonfatal",
		"task_id", taskID, "path", wtPath, "err", setupErr)
	noteErr := appendSetupFailureNote(ctx, wtPath, setupErr)
	if noteErr != nil {
		m.logger.Warn("worktree.setup-fail-note", "task_id", taskID, "path", wtPath, "err", noteErr)
	}
	markerErr := writeSetupFailureMarker(ctx, wtPath, setupErr)
	if markerErr != nil {
		m.logger.Warn("worktree.setup-fail-marker", "task_id", taskID, "path", wtPath, "err", markerErr)
	}
	if persistErr := errors.Join(noteErr, markerErr); persistErr != nil {
		return fmt.Errorf("persist setup failure context: %w", persistErr)
	}
	return nil
}

// setupLogPath returns the per-task setup log file path. Empty logsDir
// disables file logging (returns ""), keeping in-memory/test setups working
// without needing to configure a log dir.
func (m *Manager) setupLogPath(taskID string) string {
	if m.logsDir == "" {
		return ""
	}
	return filepath.Join(m.logsDir, "worktrees", taskID+"-setup.log")
}

func (m *Manager) openSetupLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	// Truncate: each worktree prep starts fresh, old log contents are stale.
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

// hasMiseConfig reports whether the worktree root contains a mise config.
// mise reads any of these names; the list mirrors mise's own discovery so
// we only spend a trust call when mise will actually care.
func hasMiseConfig(wtPath string) bool {
	names := []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml"}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(wtPath, n)); err == nil {
			return true
		}
	}
	return false
}

// resolveSetupCommands loads the worktree's .sybra.yaml (if present) and
// merges its `setup:` block with the project's app-level SetupCommands.
// Repo commands run first (canonical toolchain bootstrap), app commands
// second (per-machine additions). A missing or unparseable .sybra.yaml
// falls back to app commands only — logged but non-fatal so an agent can
// still start on a checkout without the file.
func (m *Manager) resolveSetupCommands(wtPath string, proj project.Project) []string {
	repoCfg, err := project.LoadRepoConfig(wtPath)
	if err != nil {
		m.logger.Warn("worktree.repo-config-setup",
			"path", wtPath, "project", proj.ID, "err", err)
		return proj.SetupCommands
	}
	var repoSetup []string
	if repoCfg != nil {
		repoSetup = repoCfg.Setup
	}
	merged := project.MergeSetup(repoSetup, proj.SetupCommands)
	if len(merged) > 0 {
		m.logger.Info("worktree.setup-resolved",
			"path", wtPath, "project", proj.ID,
			"repo_cmds", len(repoSetup), "app_cmds", len(proj.SetupCommands),
			"total", len(merged))
	}
	return merged
}

// resolveTrustedSetupCommands loads .sybra.yaml from the project's default
// branch in the bare clone — never the checked-out worktree — and merges its
// `setup:` block with the project's app-level SetupCommands. Use this instead
// of resolveSetupCommands whenever the worktree's checked-out ref is
// untrusted (PrepareForReview/PrepareForFix check out a PR head, which may be
// a fork or a Renovate branch): that ref's own .sybra.yaml is
// attacker-controlled, and its setup commands run via `sh -c` outside the
// agent permission model (issue #1519).
func (m *Manager) resolveTrustedSetupCommands(ctx context.Context, proj project.Project) []string {
	repoCfg, err := project.LoadRepoConfigAtDefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		m.logger.Warn("worktree.repo-config-setup-trusted",
			"project", proj.ID, "err", err)
		return proj.SetupCommands
	}
	var repoSetup []string
	if repoCfg != nil {
		repoSetup = repoCfg.Setup
	}
	merged := project.MergeSetup(repoSetup, proj.SetupCommands)
	if len(merged) > 0 {
		m.logger.Info("worktree.setup-resolved-trusted",
			"project", proj.ID,
			"repo_cmds", len(repoSetup), "app_cmds", len(proj.SetupCommands),
			"total", len(merged))
	}
	return merged
}

// desktopBuildSetupMarker matches the desktop production build step
// (`npm run build:desktop`) that .sybra.yaml setup blocks use so `go build`
// has something to //go:embed. A code-authoring role needs it; a read-only
// PR review worktree never builds anything, so running it there is pure
// waste (issue #1527).
const desktopBuildSetupMarker = "build:desktop"

// filterNonAuthoringSetup drops setup commands that exist only to prepare a
// worktree for building/embedding, for roles that never build — currently
// just PrepareForReview's detached-HEAD, read-only checkout.
func filterNonAuthoringSetup(commands []string) []string {
	filtered := make([]string, 0, len(commands))
	for _, c := range commands {
		if strings.Contains(c, desktopBuildSetupMarker) {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

func (m *Manager) installChecks(ctx context.Context, wtPath string, proj project.Project) {
	repoCfg, err := project.LoadRepoConfig(wtPath)
	if err != nil {
		m.logger.Warn("worktree.repo-config", "path", wtPath, "err", err)
	}
	var repoChecks *project.ChecksConfig
	if repoCfg != nil {
		repoChecks = repoCfg.Checks
	}
	checks := project.MergeChecks(repoChecks, proj.Checks)
	if err := project.InstallHooks(ctx, wtPath, checks); err != nil {
		m.logger.Warn("worktree.hooks", "path", wtPath, "err", err)
	}
	if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.signoff-hook", "path", wtPath, "err", err)
	}
	if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.fork-only-push", "path", wtPath, "err", err)
	}
	if err := project.ConfigureGitHubAuth(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.github-auth", "path", wtPath, "err", err)
	}
}
