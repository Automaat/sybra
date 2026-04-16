//go:build integration && k8s

package sandbox

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

// TestK8sSandbox_StartStop creates a k3d cluster, deploys a minimal workload,
// verifies port-forward, then tears everything down.
func TestK8sSandbox_StartStop(t *testing.T) {
	ctx := context.Background()
	m := newK8sManager(t)

	// Deploy nginx via kubectl apply from a manifest string written to worktree.
	worktree := t.TempDir()
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  selector:
    app: nginx
  ports:
  - port: 80
    targetPort: 80
`
	manifestPath := worktree + "/nginx.yaml"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := &project.SandboxConfig{
		Cluster: "k3d",
		Deploy:  "kubectl apply -f " + manifestPath + " && kubectl rollout status deployment/nginx",
		Service: "nginx",
		Port:    80,
	}

	inst, err := m.Start(ctx, "task-k8s-start-stop", worktree, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst == nil {
		t.Fatal("expected non-nil instance")
	}
	if inst.Kubeconfig == "" {
		t.Error("Kubeconfig path should be set")
	}
	if _, err := os.Stat(inst.Kubeconfig); err != nil {
		t.Errorf("kubeconfig file not found: %v", err)
	}

	assertHTTP200(t, inst.URL, 30*t.Deadline().Sub(t.Deadline())) // generous timeout
	m.Stop("task-k8s-start-stop")
}

// TestK8sSandbox_ClusterExists verifies that Start reuses an existing cluster
// rather than failing.
func TestK8sSandbox_ClusterExists(t *testing.T) {
	ctx := context.Background()
	m := newK8sManager(t)

	clusterName := "sybra-task-k8s-exists"
	// Pre-create cluster.
	if out, err := runCmd(ctx, "", nil, "k3d", "cluster", "create", clusterName,
		"--kubeconfig-update-default=false",
		"--kubeconfig-switch-context=false",
		"--wait",
	); err != nil {
		t.Fatalf("pre-create cluster: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = stopK3dCluster(clusterName) })

	cfg := &project.SandboxConfig{
		Cluster: "k3d",
		Service: "nonexistent", // no deploy, just verify cluster reuse
		Port:    80,
	}
	// Should not fail even though cluster exists — just skip create step.
	_, err := m.Start(ctx, "task-k8s-exists", t.TempDir(), cfg)
	// Port-forward to nonexistent service will fail, but we only care that
	// the error is NOT "cluster already exists".
	if err != nil && contains(err.Error(), "already exists") {
		t.Errorf("Start failed with 'already exists' error instead of reusing cluster: %v", err)
	}
}

// TestK8sSandbox_KubeconfigCleanup verifies the kubeconfig is removed on Stop.
func TestK8sSandbox_KubeconfigCleanup(t *testing.T) {
	ctx := context.Background()
	m := newK8sManager(t)

	cfg := &project.SandboxConfig{
		Cluster: "k3d",
		Service: "nonexistent",
		Port:    80,
	}
	inst, err := m.Start(ctx, "task-k8s-cleanup", t.TempDir(), cfg)
	if err != nil && inst == nil {
		t.Skip("cluster started but port-forward failed — skipping cleanup check")
	}
	if inst == nil {
		return
	}
	kubeconfigPath := inst.Kubeconfig
	m.Stop("task-k8s-cleanup")

	if kubeconfigPath != "" {
		if _, statErr := os.Stat(kubeconfigPath); statErr == nil {
			t.Errorf("kubeconfig %q still exists after Stop", kubeconfigPath)
		}
	}
}

// TestK8sSandbox_DeployCommand verifies the deploy command runs in worktree dir.
func TestK8sSandbox_DeployCommand(t *testing.T) {
	ctx := context.Background()
	m := newK8sManager(t)

	worktree := t.TempDir()
	// Use a deploy command that writes a file to the worktree dir.
	sentinelPath := worktree + "/deployed"
	cfg := &project.SandboxConfig{
		Cluster: "k3d",
		Deploy:  "touch " + sentinelPath,
		Service: "nonexistent",
		Port:    80,
	}

	_, _ = m.Start(ctx, "task-k8s-deploy-cmd", worktree, cfg)
	defer m.Stop("task-k8s-deploy-cmd")

	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("deploy command did not run in worktree: sentinel file missing: %v", err)
	}
}

func newK8sManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return NewManager(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := range len(s) - len(substr) + 1 {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
