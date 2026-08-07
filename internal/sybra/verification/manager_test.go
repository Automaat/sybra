package verification

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
)

func TestConcurrentVerificationWorkspacesAreIndependent(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	leases := make([]Lease, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = mgr.Prepare(context.Background(), "task-concurrent", "verify", canonical)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare %d: %v", i, err)
		}
		t.Cleanup(func() { mgr.Release(leases[i]) })
	}
	if leases[0].WorkspaceDir == leases[1].WorkspaceDir {
		t.Fatal("concurrent runs shared a workspace")
	}
	if leases[0].ScratchDir == leases[1].ScratchDir {
		t.Fatal("concurrent runs shared sandbox/provider state")
	}
	if err := os.WriteFile(filepath.Join(leases[0].WorkspaceDir, "only-first"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(leases[1].WorkspaceDir, "only-first")); !os.IsNotExist(err) {
		t.Fatalf("write crossed verification workspaces: %v", err)
	}
	if _, err := os.Stat(filepath.Join(canonical, "only-first")); !os.IsNotExist(err) {
		t.Fatalf("write reached canonical worktree: %v", err)
	}
}

func TestConcurrentScratchOnlyVerifierLeasesAreIndependentAndReleasable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "verification")
	mgr := New(root, nil, nil)
	one, err := mgr.PrepareScratch("task-judge", "review")
	if err != nil {
		t.Fatal(err)
	}
	two, err := mgr.PrepareScratch("task-judge", "review")
	if err != nil {
		t.Fatal(err)
	}
	if one.ScratchDir == two.ScratchDir || one.WorkspaceDir != "" || two.WorkspaceDir != "" {
		t.Fatalf("scratch-only leases share state or own a source clone: one=%+v two=%+v", one, two)
	}
	mgr.Release(one)
	if _, err := os.Stat(one.ScratchDir); !os.IsNotExist(err) {
		t.Fatalf("released scratch remains: %v", err)
	}
	if _, err := os.Stat(two.ScratchDir); err != nil {
		t.Fatalf("releasing one lease removed its sibling: %v", err)
	}
	mgr.Release(two)
}

func TestDisposableWorkspaceIsWritableAndNeverMutatesCanonical(t *testing.T) {
	canonical := initRepo(t)
	store := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	mgr := New(filepath.Join(t.TempDir(), "verification"), store, nil)
	lease, err := mgr.Prepare(context.Background(), "task-1", "test-runner", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })

	if err := os.WriteFile(filepath.Join(lease.WorkspaceDir, "generated.txt"), []byte("verifier output\n"), 0o600); err != nil {
		t.Fatalf("workspace should be writable: %v", err)
	}
	git(t, lease.WorkspaceDir, "add", "generated.txt")
	git(t, lease.WorkspaceDir,
		"-c", "user.name=Sybra Test",
		"-c", "user.email=test@example.invalid",
		"commit", "-m", "private verifier commit")
	if err := os.WriteFile(filepath.Join(lease.WorkspaceDir, "fixture.untracked"), []byte("ephemeral fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(canonical, "generated.txt")); !os.IsNotExist(err) {
		t.Fatalf("canonical worktree was mutated: %v", err)
	}
	if got := strings.TrimSpace(git(t, canonical, "rev-parse", "HEAD")); got != lease.SourceSHA {
		t.Fatalf("canonical HEAD moved to %s, want %s", got, lease.SourceSHA)
	}
	if err := mgr.Finalize(context.Background(), lease, []string{"go test ./..."}, "ok", "cert-1"); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("task-1")
	if err != nil || len(items) != 1 {
		t.Fatalf("verification artifact = %v, %v", items, err)
	}
	content, _, err := store.Read("task-1", items[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"certificateId": "cert-1"`) || !strings.Contains(string(content), `"status": "accepted"`) {
		t.Fatalf("artifact lacks provenance: %s", content)
	}
	if !strings.Contains(string(content), "generated.txt") {
		t.Fatalf("artifact did not capture verifier commit diff: %s", content)
	}
	if !strings.Contains(string(content), "fixture.untracked") || !strings.Contains(string(content), "ephemeral fixture") {
		t.Fatalf("artifact did not capture untracked verifier output: %s", content)
	}
}

func TestFinalizeRejectsMovedSource(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	lease, err := mgr.Prepare(context.Background(), "task-1", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	if err := os.WriteFile(filepath.Join(canonical, "source.txt"), []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, canonical, "add", "source.txt")
	git(t, canonical, "commit", "-m", "move source")
	if err := mgr.Finalize(context.Background(), lease, nil, "", ""); !errors.Is(err, ErrSourceMoved) {
		t.Fatalf("Finalize error = %v, want ErrSourceMoved", err)
	}
}

func TestFinalizeRejectsDirtySource(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	lease, err := mgr.Prepare(t.Context(), "task-dirty", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	if err := os.WriteFile(filepath.Join(canonical, "uncommitted.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Finalize(t.Context(), lease, nil, "", ""); !errors.Is(err, ErrSourceDirty) {
		t.Fatalf("Finalize error = %v, want ErrSourceDirty", err)
	}
}

func TestFinalizeRejectsABASourceMovement(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	lease, err := mgr.Prepare(t.Context(), "task-aba", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	if err := os.WriteFile(filepath.Join(canonical, "source.txt"), []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, canonical, "add", "source.txt")
	git(t, canonical, "commit", "-m", "temporary movement")
	git(t, canonical, "reset", "--hard", lease.SourceSHA)
	if err := mgr.Finalize(t.Context(), lease, nil, "", ""); !errors.Is(err, ErrSourceMoved) {
		t.Fatalf("Finalize error = %v, want ErrSourceMoved after A-B-A movement", err)
	}
}

func TestFinalizeIgnoresUnrelatedRefMovement(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	lease, err := mgr.Prepare(t.Context(), "task-stable", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	git(t, canonical, "branch", "unrelated-task")
	if err := mgr.Finalize(t.Context(), lease, nil, "", ""); err != nil {
		t.Fatalf("unrelated ref invalidated task lease: %v", err)
	}
}

func TestFinalizeUsesCertificateAttachedByPreflight(t *testing.T) {
	canonical := initRepo(t)
	store := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	mgr := New(filepath.Join(t.TempDir(), "verification"), store, nil)
	lease, err := mgr.Prepare(t.Context(), "task-cert", "verify", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	if err := mgr.SetCertificateForWorkspace(lease.WorkspaceDir, "cert-from-preflight"); err != nil {
		t.Fatal(err)
	}
	lease, err = mgr.Lease(lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Finalize(t.Context(), lease, []string{"true"}, "ok", ""); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("task-cert")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("finalize did not persist a verification artifact")
	}
	content, _, err := store.Read("task-cert", items[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"certificateId": "cert-from-preflight"`) {
		t.Fatalf("report omitted preflight certificate: %s", content)
	}
}

func TestFinalizeRejectsDestroyedWorkspaceGitMetadata(t *testing.T) {
	canonical := initRepo(t)
	store := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	mgr := New(filepath.Join(t.TempDir(), "verification"), store, nil)
	lease, err := mgr.Prepare(context.Background(), "task-invalid", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Release(lease) })
	if err := os.RemoveAll(filepath.Join(lease.WorkspaceDir, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Finalize(context.Background(), lease, nil, "verdict", "cert"); err == nil {
		t.Fatal("Finalize accepted a workspace whose Git provenance was destroyed")
	}
	items, err := store.List("task-invalid")
	if err != nil || len(items) != 1 {
		t.Fatalf("invalid verification artifact = %v, %v", items, err)
	}
	content, _, err := store.Read("task-invalid", items[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"status": "invalid"`) {
		t.Fatalf("invalid workspace was not marked invalid: %s", content)
	}
}

func TestReconcileAdoptsLiveLeaseAndCleansAbandonedLease(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	var revoked []string
	mgr.SetGrantRevoker(func(scratchDir string) error {
		revoked = append(revoked, scratchDir)
		return nil
	})
	live, err := mgr.Prepare(context.Background(), "task-live", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := mgr.Prepare(context.Background(), "task-dead", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.BindAgent(live.ID, "agent-live"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.BindAgent(dead.ID, "agent-dead"); err != nil {
		t.Fatal(err)
	}
	mgr.Reconcile(map[string]struct{}{"agent-live": {}})
	if _, err := os.Stat(live.WorkspaceDir); err != nil {
		t.Fatalf("live workspace removed: %v", err)
	}
	if _, err := os.Stat(dead.WorkspaceDir); !os.IsNotExist(err) {
		t.Fatalf("abandoned workspace still exists: %v", err)
	}
	if len(revoked) != 1 || revoked[0] != dead.ScratchDir {
		t.Fatalf("revoked scratch paths = %v, want [%q]", revoked, dead.ScratchDir)
	}
	mgr.Release(live)
}

func TestReconcileRetainsLeaseWhenGrantRevocationFails(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	dead, err := mgr.Prepare(context.Background(), "task-dead", "review", canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.BindAgent(dead.ID, "agent-dead"); err != nil {
		t.Fatal(err)
	}
	mgr.SetGrantRevoker(func(string) error { return errors.New("persist failed") })

	mgr.Reconcile(nil)
	if _, err := os.Stat(dead.WorkspaceDir); err != nil {
		t.Fatalf("workspace removed before durable grant revocation: %v", err)
	}
	if _, err := mgr.Lease(dead.ID); err != nil {
		t.Fatalf("lease removed before durable grant revocation: %v", err)
	}

	mgr.SetGrantRevoker(func(string) error { return nil })
	mgr.Reconcile(nil)
	if _, err := os.Stat(dead.WorkspaceDir); !os.IsNotExist(err) {
		t.Fatalf("workspace retained after successful retry: %v", err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "Sybra Test")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "source.txt")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
