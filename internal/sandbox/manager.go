// Package sandbox manages isolated app environments (docker or k8s) for tasks.
// Each task gets at most one sandbox instance; Start is idempotent.
package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Instance is a running sandbox tied to a single task.
type Instance struct {
	TaskID     string
	URL        string // http://localhost:<hostPort>
	Kubeconfig string // absolute path; empty for docker mode

	// docker mode
	composeArgs []string // [-f <file> -p <project>] reused for down
	entryFile   string   // generated compose file path (empty if using existing)

	// k8s mode
	portFwdCmd     *exec.Cmd
	clusterName    string
	kubeconfigPath string
}

// EnvVars returns the environment variable pairs to inject into the agent subprocess.
func (i *Instance) EnvVars() []string {
	if i == nil {
		return nil
	}
	vars := []string{fmt.Sprintf("SANDBOX_URL=%s", i.URL)}
	if i.Kubeconfig != "" {
		vars = append(vars, fmt.Sprintf("KUBECONFIG=%s", i.Kubeconfig))
	}
	return vars
}

// Manager holds all running sandbox instances keyed by task ID.
type Manager struct {
	mu           sync.Mutex
	instances    map[string]*Instance
	starting     map[string]chan struct{} // taskID -> closed when its in-flight Start finishes
	startSandbox func(context.Context, string, string, *project.SandboxConfig) (*Instance, error)
	logger       *slog.Logger
	dataDir      string        // e.g. ~/.sybra/sandboxes
	retention    time.Duration // see SetRetentionWindow

	// normalizeOwnership and removeAll are overridden in tests; nil defaults
	// to dockerChownNormalizer and fsutil.RemoveAllForce respectively.
	normalizeOwnership ownershipNormalizer
	removeAll          func(string) error
}

// NewManager creates a Manager that stores per-task files under dataDir.
func NewManager(dataDir string, logger *slog.Logger) *Manager {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logger.Warn("sandbox.manager.datadir", "err", err)
	}
	return &Manager{
		instances: make(map[string]*Instance),
		starting:  make(map[string]chan struct{}),
		logger:    logger,
		dataDir:   dataDir,
	}
}

// SetRetentionWindow configures how long CleanupOrphaned waits, after a
// task becomes cleanup-eligible (see cleanupEligible), before removing its
// sandbox dir. d == 0 (the Manager's zero value) removes eligible dirs
// immediately, matching pre-retention behavior. d < 0 disables age-based
// pruning entirely — eligible dirs are only removed once their task is
// deleted. d > 0 requires the task to have been eligible for at least d.
func (m *Manager) SetRetentionWindow(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retention = d
}

// Get returns the running instance for a task, or nil if none exists.
func (m *Manager) Get(taskID string) *Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[taskID]
}

// Start ensures a sandbox is running for the given task. Idempotent: if one
// is already running the existing instance is returned. Concurrent Start
// calls for the same taskID single-flight through the starting map - only
// the first launches the sandbox, the rest wait for it and reuse its
// result - so two racing callers can never both pass the "does it exist
// yet" check and boot a duplicate cluster (issue #1538). Returns an error
// if cfg is nil. Failed starts are not cached: waiters wake, re-check state,
// and may make their own start attempt.
func (m *Manager) Start(ctx context.Context, taskID, worktreePath string, cfg *project.SandboxConfig) (inst *Instance, err error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil sandbox config")
	}

	var startCh chan struct{}
	for {
		m.mu.Lock()
		if inst, ok := m.instances[taskID]; ok {
			m.mu.Unlock()
			m.logger.Info("sandbox.reuse", "task_id", taskID, "url", inst.URL)
			return inst, nil
		}
		if ch, ok := m.starting[taskID]; ok {
			m.mu.Unlock()
			select {
			case <-ch:
				continue // re-check instances/starting under lock
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		m.starting[taskID] = ch
		startCh = ch
		m.mu.Unlock()
		break
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("sandbox start panic: %v", recovered)
			m.logger.Error("sandbox.start.panic", "task_id", taskID, "panic", recovered, "stack", string(debug.Stack()))
		}
		m.mu.Lock()
		if m.starting[taskID] == startCh {
			delete(m.starting, taskID)
		}
		if err == nil && inst != nil {
			m.instances[taskID] = inst
		}
		m.mu.Unlock()
		close(startCh)
	}()

	starter := m.startSandbox
	if starter == nil {
		starter = m.defaultStartSandbox
	}
	inst, err = starter(ctx, taskID, worktreePath, cfg)

	if err != nil {
		return nil, err
	}
	if inst == nil {
		err = fmt.Errorf("sandbox start returned nil instance")
		return nil, err
	}

	m.logger.Info("sandbox.started", "task_id", taskID, "url", inst.URL)
	return inst, nil
}

func (m *Manager) defaultStartSandbox(ctx context.Context, taskID, worktreePath string, cfg *project.SandboxConfig) (*Instance, error) {
	if cfg.IsK8s() {
		return m.startK8s(ctx, taskID, worktreePath, cfg)
	}
	return m.startDocker(ctx, taskID, worktreePath, cfg)
}

// Stop tears down the sandbox for a task. No-op if no sandbox is running.
func (m *Manager) Stop(taskID string) {
	m.StopContext(context.Background(), taskID)
}

// StopContext tears down the sandbox for a task using ctx for any external
// teardown commands. No-op if no sandbox is running.
func (m *Manager) StopContext(ctx context.Context, taskID string) {
	m.mu.Lock()
	inst, ok := m.instances[taskID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.instances, taskID)
	m.mu.Unlock()

	m.logger.Info("sandbox.stopping", "task_id", taskID)
	if inst.clusterName != "" {
		m.stopK8s(ctx, inst)
	} else {
		m.stopDocker(ctx, inst)
	}
	m.logger.Info("sandbox.stopped", "task_id", taskID)
}

// Remove tears down any running sandbox for taskID, then removes its per-task
// data dir under Manager.dataDir. Safe to call when no sandbox is running.
func (m *Manager) Remove(taskID string) {
	m.RemoveContext(context.Background(), taskID)
}

// RemoveContext tears down any running sandbox for taskID using ctx for any
// external teardown commands, then removes its per-task data dir under
// Manager.dataDir. Safe to call when no sandbox is running.
//
// Removal escalates through three steps before giving up:
//  1. RemoveAllForce's own best-effort chmod-and-retry pass (handles a
//     read-only bit left by a build/cache tool the host process itself owns).
//  2. Ownership normalization through the privileged cleanup boundary (see
//     dockerChownNormalizer) when any entry's UID/GID no longer matches the
//     record written at creation (see prepareTaskDir) — e.g. a docker/k8s
//     sandbox wrote host-visible files as root via a bind mount, which a
//     plain chmod cannot fix without CAP_CHOWN.
//  3. A bounded backoff retry for transient busy errors (EBUSY/ETXTBSY),
//     which commonly follow StopContext killing a container/cluster whose
//     mount hasn't fully released yet.
//
// A failure that survives all three is quarantined (see QuarantineEntry)
// instead of being retried on every future CleanupOrphaned tick, and
// reported via Manager.QuarantinedEntries.
func (m *Manager) RemoveContext(ctx context.Context, taskID string) {
	m.StopContext(ctx, taskID)
	taskDir, err := m.taskDir(taskID)
	if err != nil {
		m.logger.Warn("sandbox.remove.path", "task_id", taskID, "err", err)
		return
	}
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		m.clearQuarantine(taskID)
		return
	}

	if err := m.removeSandboxDir(ctx, taskID, taskDir); err != nil {
		m.quarantine(taskID, taskDir, err)
		return
	}
	m.clearQuarantine(taskID)
	m.logger.Info("sandbox.removed", "task_id", taskID, "path", taskDir)
}

// removeSandboxDir performs the normalize-then-retry escalation described on
// RemoveContext, returning the final error (if any) for the caller to
// quarantine.
func (m *Manager) removeSandboxDir(ctx context.Context, taskID, taskDir string) error {
	rec, hasOwnerRecord := cleanupOwnerRecord(taskDir)
	normalized := false
	if mismatchedOwnership(taskDir, rec) {
		normalized = m.normalizeSandboxOwnership(ctx, taskID, taskDir, rec)
	}

	removeAll := m.removeAll
	if removeAll == nil {
		removeAll = fsutil.RemoveAllForce
	}

	err := removeAll(taskDir)
	if err != nil && !hasOwnerRecord && !normalized && isPermissionRemoveErr(err) {
		normalized = m.normalizeSandboxOwnership(ctx, taskID, taskDir, rec)
		if normalized {
			err = removeAll(taskDir)
		}
	}
	for attempt := 0; err != nil && isTransientRemoveErr(err) && attempt < len(sandboxRemoveBackoffs); attempt++ {
		m.logger.Warn("sandbox.remove.retry", "task_id", taskID, "attempt", attempt+1, "err", err)
		sandboxRemoveSleep(sandboxRemoveBackoffs[attempt])
		err = removeAll(taskDir)
	}
	return err
}

func (m *Manager) normalizeSandboxOwnership(ctx context.Context, taskID, taskDir string, rec ownerRecord) bool {
	normalize := m.normalizeOwnership
	if normalize == nil {
		normalize = dockerChownNormalizer
	}
	if err := normalize(ctx, taskDir, rec.UID, rec.GID); err != nil {
		m.logger.Warn("sandbox.remove.normalize", "task_id", taskID, "path", taskDir, "err", err)
		return false
	}
	return true
}

// quarantine records taskDir as a genuinely unsafe (or at least not
// automatically recoverable) cleanup failure, bumping the attempt count and
// preserving the original first-failure time if a record already exists.
func (m *Manager) quarantine(taskID, taskDir string, cause error) {
	size, sizeErr := dirSize(taskDir)
	if sizeErr != nil {
		m.logger.Warn("sandbox.quarantine.size", "task_id", taskID, "path", taskDir, "err", sizeErr)
	}

	now := time.Now()
	entry := QuarantineEntry{
		TaskID:        taskID,
		Path:          taskDir,
		BytesRetained: size,
		Attempts:      1,
		LastError:     cause.Error(),
		FirstFailedAt: now,
		LastFailedAt:  now,
	}
	if existing, ok := m.loadQuarantine(taskID); ok {
		entry.Attempts = existing.Attempts + 1
		entry.FirstFailedAt = existing.FirstFailedAt
	}
	if err := m.saveQuarantine(entry); err != nil {
		m.logger.Warn("sandbox.quarantine.save", "task_id", taskID, "err", err)
	}
	m.logger.Error("sandbox.remove.quarantined", "task_id", taskID, "path", taskDir,
		"bytes_retained", size, "attempts", entry.Attempts, "err", cause)
}

// cleanupEligible reports whether a task's sandbox dir is a candidate for
// retention-based cleanup. Deliberately broader than task.IsTerminalStatus
// (which omits blocked): a blocked task has no live agent and cannot resume
// without a status change, so its sandbox is just as safe to age out as a
// done/cancelled one.
func cleanupEligible(s task.Status) bool {
	return s == task.StatusDone || s == task.StatusCancelled || s == task.StatusBlocked
}

// CleanupOrphaned removes per-task sandbox dirs for deleted tasks
// immediately, and for cleanup-eligible tasks (see cleanupEligible) once
// they have been eligible for at least the configured retention window (see
// SetRetentionWindow). Non-eligible tasks (active, in-review, etc.) always
// keep their dirs. When hasAgent reports a live task agent, the dir is
// preserved so cleanup never races an active run.
func (m *Manager) CleanupOrphaned(ctx context.Context, tasks []task.Task, hasAgent func(string) bool) {
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		return
	}

	active := make(map[string]task.Task, len(tasks))
	for i := range tasks {
		active[tasks[i].ID] = tasks[i]
	}

	m.mu.Lock()
	retention := m.retention
	m.mu.Unlock()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		if taskID == quarantineDirName {
			continue
		}
		t, exists := active[taskID]
		switch {
		case hasAgent != nil && hasAgent(taskID):
			// A live agent takes precedence over every other signal,
			// including a task record that's gone missing — an orphaned
			// dir with a running agent is not orphaned, it's just
			// unlinked from the (possibly stale) task list.
			continue
		case !exists:
			// Deleted task — remove regardless of age, and regardless of any
			// prior quarantine: an explicit delete deserves one more attempt.
		case !cleanupEligible(t.Status):
			continue
		default:
			if _, quarantined := m.loadQuarantine(taskID); quarantined {
				// Already reported via health.checkSandboxCleanupFailures;
				// retrying every tick would just repeat the same failing
				// normalize+remove work for no progress.
				continue
			}
			switch {
			case retention < 0:
				// Age-based pruning disabled — eligible dirs wait for task deletion.
				continue
			case retention > 0:
				staleSince := t.StatusChangedAt
				if staleSince.IsZero() {
					if info, err := e.Info(); err == nil {
						staleSince = info.ModTime()
					}
				}
				if staleSince.IsZero() || time.Since(staleSince) < retention {
					continue
				}
			}
		}
		m.RemoveContext(ctx, taskID)
	}
}

// SybraHomeDir returns (creating on first call) an isolated, empty directory
// under dataDir a test-runner/eval agent should point SYBRA_HOME at when the
// task under test is Sybra itself. Unlike Start, this needs no Docker/k8s
// machinery — plain isolation of the app-under-test's data dir is all
// Sybra's own startup needs, since it creates tasks/, logs/, etc. itself
// against a fresh home (see docs/manual-testing.md). Kept on Manager anyway
// so per-task sandbox state lives under one dataDir regardless of kind.
func (m *Manager) SybraHomeDir(taskID string) (string, error) {
	taskDir, err := m.prepareTaskDir(taskID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(taskDir, "sybra-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir sybra home: %w", err)
	}
	return dir, nil
}

// prepareTaskDir returns (creating on first call) the per-task sandbox dir
// under dataDir, recording the creating process's UID/GID (see
// writeOwnerRecord) the first time it is created so RemoveContext can later
// detect ownership drift left by a docker/k8s sandbox.
func (m *Manager) prepareTaskDir(taskID string) (string, error) {
	dir, err := m.taskDir(taskID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sandbox dir: %w", err)
	}
	if _, ok := readOwnerRecord(dir); !ok {
		if err := writeOwnerRecord(dir); err != nil {
			m.logger.Warn("sandbox.owner.write", "task_id", taskID, "err", err)
		}
	}
	return dir, nil
}

func (m *Manager) taskDir(taskID string) (string, error) {
	root := filepath.Clean(m.dataDir)
	dir := filepath.Clean(filepath.Join(root, taskID))
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("task dir rel: %w", err)
	}
	if rel == ".." || filepath.IsAbs(rel) || rel == "." || rel == "" {
		return "", fmt.Errorf("invalid task sandbox path for %q", taskID)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("task sandbox path escapes root for %q", taskID)
	}
	return dir, nil
}
