package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/project"
)

func (m *Manager) startK8s(ctx context.Context, taskID, worktreePath string, cfg *project.SandboxConfig) (*Instance, error) {
	clusterName := "synapse-" + taskID
	taskDir := filepath.Join(m.dataDir, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("sandbox dir: %w", err)
	}
	kubeconfigPath := filepath.Join(taskDir, "kubeconfig")

	// Check if cluster already exists (survives Synapse restarts).
	exists, err := k3dClusterExists(ctx, clusterName)
	if err != nil {
		m.logger.Warn("sandbox.k8s.cluster-check", "task_id", taskID, "err", err)
	}

	if !exists {
		out, createErr := runCmd(ctx, "", nil, "k3d", "cluster", "create", clusterName,
			"--kubeconfig-update-default=false",
			"--kubeconfig-switch-context=false",
			"--wait",
		)
		if createErr != nil {
			return nil, fmt.Errorf("k3d cluster create: %w\n%s", createErr, out)
		}
	}

	// Write kubeconfig.
	kubeconfigData, err := runCmd(ctx, "", nil, "k3d", "kubeconfig", "get", clusterName)
	if err != nil {
		_ = stopK3dCluster(clusterName)
		return nil, fmt.Errorf("k3d kubeconfig get: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfigData), 0o600); err != nil {
		_ = stopK3dCluster(clusterName)
		return nil, fmt.Errorf("write kubeconfig: %w", err)
	}

	// Load env file entries.
	envFileEntries, envFileErr := LoadEnvFile(cfg.EnvFile)
	if envFileErr != nil {
		m.logger.Warn("sandbox.envfile.load", "task_id", taskID, "err", envFileErr)
	}

	// Run deploy command in worktree with KUBECONFIG set.
	if cfg.Deploy != "" {
		deployEnv := append([]string{fmt.Sprintf("KUBECONFIG=%s", kubeconfigPath)}, envFileEntries...)
		out, deployErr := runCmd(ctx, worktreePath, deployEnv, "sh", "-c", cfg.Deploy)
		if deployErr != nil {
			_ = stopK3dCluster(clusterName)
			_ = os.Remove(kubeconfigPath)
			return nil, fmt.Errorf("deploy command %q: %w\n%s", cfg.Deploy, deployErr, out)
		}
	}

	// Find a free local port.
	localPort, err := findFreePort()
	if err != nil {
		_ = stopK3dCluster(clusterName)
		_ = os.Remove(kubeconfigPath)
		return nil, fmt.Errorf("find free port: %w", err)
	}

	// Start kubectl port-forward as a background process.
	svcName := cfg.Service
	if svcName == "" {
		svcName = "app"
	}
	pfArgs := []string{
		"port-forward",
		fmt.Sprintf("svc/%s", svcName),
		fmt.Sprintf("%d:%d", localPort, cfg.Port),
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		"--namespace=default",
	}
	pfCmd := exec.CommandContext(ctx, "kubectl", pfArgs...)
	pfCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeconfigPath))
	if err := pfCmd.Start(); err != nil {
		_ = stopK3dCluster(clusterName)
		_ = os.Remove(kubeconfigPath)
		return nil, fmt.Errorf("kubectl port-forward: %w", err)
	}

	// Wait briefly for port-forward to become ready.
	time.Sleep(2 * time.Second)

	return &Instance{
		TaskID:         taskID,
		URL:            fmt.Sprintf("http://localhost:%d", localPort),
		Kubeconfig:     kubeconfigPath,
		portFwdCmd:     pfCmd,
		clusterName:    clusterName,
		kubeconfigPath: kubeconfigPath,
	}, nil
}

func (m *Manager) stopK8s(inst *Instance) {
	// Kill port-forward.
	if inst.portFwdCmd != nil && inst.portFwdCmd.Process != nil {
		_ = inst.portFwdCmd.Process.Kill()
		_ = inst.portFwdCmd.Wait()
	}

	// Delete cluster.
	if inst.clusterName != "" {
		if err := stopK3dCluster(inst.clusterName); err != nil {
			m.logger.Warn("sandbox.k8s.cluster-delete", "task_id", inst.TaskID, "err", err)
		}
	}

	// Remove kubeconfig file and task dir.
	if inst.kubeconfigPath != "" {
		_ = os.Remove(inst.kubeconfigPath)
		_ = os.Remove(filepath.Dir(inst.kubeconfigPath))
	}
}

func k3dClusterExists(ctx context.Context, name string) (bool, error) {
	out, err := runCmd(ctx, "", nil, "k3d", "cluster", "list", "--output", "json")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, `"`+name+`"`), nil
}

func stopK3dCluster(name string) error {
	out, err := runCmd(context.Background(), "", nil, "k3d", "cluster", "delete", name)
	if err != nil {
		return fmt.Errorf("k3d cluster delete %s: %w\n%s", name, err, out)
	}
	return nil
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	if !ok {
		return 0, fmt.Errorf("unexpected listener addr type")
	}
	return addr.Port, nil
}
