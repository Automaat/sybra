// Package sandbox manages isolated app environments (docker or k8s) for tasks.
// Each task gets at most one sandbox instance; Start is idempotent.
package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/Automaat/synapse/internal/project"
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
	mu        sync.Mutex
	instances map[string]*Instance
	logger    *slog.Logger
	dataDir   string // e.g. ~/.synapse/sandboxes
}

// NewManager creates a Manager that stores per-task files under dataDir.
func NewManager(dataDir string, logger *slog.Logger) *Manager {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logger.Warn("sandbox.manager.datadir", "err", err)
	}
	return &Manager{
		instances: make(map[string]*Instance),
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
// is already running the existing instance is returned. Returns an error if cfg is nil.
func (m *Manager) Start(ctx context.Context, taskID, worktreePath string, cfg *project.SandboxConfig) (*Instance, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil sandbox config")
	}
	m.mu.Lock()
	if inst, ok := m.instances[taskID]; ok {
		m.mu.Unlock()
		m.logger.Info("sandbox.reuse", "task_id", taskID, "url", inst.URL)
		return inst, nil
	}
	m.mu.Unlock()

	var (
		inst *Instance
		err  error
	)
	if cfg.IsK8s() {
		inst, err = m.startK8s(ctx, taskID, worktreePath, cfg)
	} else {
		inst, err = m.startDocker(ctx, taskID, worktreePath, cfg)
	}
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.instances[taskID] = inst
	m.mu.Unlock()

	m.logger.Info("sandbox.started", "task_id", taskID, "url", inst.URL)
	return inst, nil
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
