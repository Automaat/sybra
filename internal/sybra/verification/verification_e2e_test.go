//go:build e2e

package verification

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/task"
)

func TestE2EConcurrentGitWritingVerificationDoesNotMutateAuthoritativeCheckout(t *testing.T) {
	canonical := initRepo(t)
	mgr := New(filepath.Join(t.TempDir(), "verification"), artifact.New(filepath.Join(t.TempDir(), "artifacts")), nil)
	leases := make([]Lease, 2)
	errCh := make(chan error, len(leases))
	var wg sync.WaitGroup
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := mgr.Prepare(context.Background(), "task-e2e", fmt.Sprintf("verify-%d", i), canonical)
			if err != nil {
				errCh <- err
				return
			}
			leases[i] = lease
			if err := os.WriteFile(filepath.Join(lease.WorkspaceDir, "generated.txt"), []byte(fmt.Sprintf("generated-%d\n", i)), 0o600); err != nil {
				errCh <- err
				return
			}
			store, err := task.NewStore(filepath.Join(lease.WorkspaceDir, "task-fixtures"))
			if err != nil {
				errCh <- err
				return
			}
			if _, err := store.Create("fixture", "body", task.AgentModeHeadless); err != nil {
				errCh <- err
				return
			}
			git(t, lease.WorkspaceDir, "add", "generated.txt", "task-fixtures")
			git(t, lease.WorkspaceDir,
				"-c", "user.name=Sybra Test",
				"-c", "user.email=test@example.invalid",
				"commit", "-m", "private verifier fixture")
			if err := mgr.Finalize(context.Background(), lease, []string{"generate", "git-writing-test", "task-store-fixture"}, "pass", "cert-e2e"); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if status := git(t, canonical, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("authoritative checkout mutated:\n%s", status)
	}
	for _, lease := range leases {
		mgr.Release(lease)
		if _, err := os.Stat(lease.WorkspaceDir); !os.IsNotExist(err) {
			t.Fatalf("verification workspace survived release: %s (%v)", lease.WorkspaceDir, err)
		}
	}
}
