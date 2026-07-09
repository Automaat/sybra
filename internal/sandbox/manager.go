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
	"sync"

	"github.com/Automaat/sybra/internal/project"
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
	dataDir      string // e.g. ~/.sybra/sandboxes
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
		m.stopK8s(inst)
	} else {
		m.stopDocker(inst)
	}
	m.logger.Info("sandbox.stopped", "task_id", taskID)
}

// SybraHomeDir returns (creating on first call) an isolated, empty directory
// under dataDir a test-runner/eval agent should point SYBRA_HOME at when the
// task under test is Sybra itself. Unlike Start, this needs no Docker/k8s
// machinery — plain isolation of the app-under-test's data dir is all
// Sybra's own startup needs, since it creates tasks/, logs/, etc. itself
// against a fresh home (see docs/manual-testing.md). Kept on Manager anyway
// so per-task sandbox state lives under one dataDir regardless of kind.
func (m *Manager) SybraHomeDir(taskID string) (string, error) {
	dir := filepath.Join(m.dataDir, taskID, "sybra-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir sybra home: %w", err)
	}
	return dir, nil
}
