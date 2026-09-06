package verification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/reviewprogress"
)

const checkpoint = reviewprogress.Start + `{"inspected":["source.txt"],"findings":["provisional-lock-concern"],"remaining":["race retry"]}` + reviewprogress.End

func progressScope() ProgressScope {
	return ProgressScope{Lineage: strings.Repeat("a", 64), ContractDigest: strings.Repeat("b", 64), BaseRef: "HEAD"}
}

func progressLease(t *testing.T, m *Manager, repo, taskID, role, agentID string, scope ProgressScope) (Lease, string) {
	t.Helper()
	lease, err := m.Prepare(t.Context(), taskID, role, repo)
	if err != nil {
		t.Fatal(err)
	}
	lease, seed, err := m.PrepareProgress(t.Context(), lease, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgent(lease.ID, agentID); err != nil {
		t.Fatal(err)
	}
	lease, err = m.Lease(lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Release(lease) })
	return lease, seed
}

func TestReviewProgressResumesAfterManagerRestart(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(t.TempDir(), "verification")
	m := New(root, nil, nil)
	scope := progressScope()
	one, seed := progressLease(t, m, repo, "task", "review", "attempt-one", scope)
	if strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("new reviewer inherited progress")
	}
	if err := m.CaptureProgress(t.Context(), one, "attempt-one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	m.Release(one)
	restarted := New(root, nil, nil)
	two, seed := progressLease(t, restarted, repo, "task", "review", "attempt-two", scope)
	if !strings.Contains(seed, "provisional-lock-concern") || !strings.Contains(seed, "never authorizes CLEAN") {
		t.Fatalf("missing bounded advisory resume: %s", seed)
	}
	if two.SourceSHA != one.SourceSHA || two.WorkspaceDir == one.WorkspaceDir {
		t.Fatal("retry did not use fresh verifier clone of same input")
	}
	if strings.Contains(restarted.progressPath(*two.ProgressInput), repo) || strings.Contains(restarted.progressPath(*two.ProgressInput), two.ScratchDir) {
		t.Fatal("checkpoint stored in agent-writable source/scratch")
	}
	info, err := os.Stat(restarted.progressPath(*two.ProgressInput))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint permissions: %v %v", info, err)
	}
	if err := restarted.CaptureProgress(t.Context(), two, "attempt-two", "review", nil, true); err != nil {
		t.Fatal(err)
	}
	_, seed = progressLease(t, restarted, repo, "task", "review", "fresh-review", scope)
	if strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("completed review leaked provisional notes to fresh review")
	}
}

func TestReviewProgressRejectsOtherIdentityAndInputs(t *testing.T) {
	repo := initRepo(t)
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	scope := progressScope()
	one, _ := progressLease(t, m, repo, "task", "review", "one", scope)
	if err := m.CaptureProgress(t.Context(), one, "wrong-agent", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	if err := m.CaptureProgress(t.Context(), one, "one", "implementation", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.progressPath(*one.ProgressInput)); !os.IsNotExist(err) {
		t.Fatal("unrelated author wrote reviewer progress")
	}
	if err := m.CaptureProgress(t.Context(), one, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, taskID, role string
		scope              ProgressScope
	}{
		{"task", "other", "review", scope},
		{"role", "task", "test-runner", scope},
		{"lineage", "task", "review", ProgressScope{Lineage: strings.Repeat("c", 64), ContractDigest: scope.ContractDigest, BaseRef: scope.BaseRef}},
		{"contract", "task", "review", ProgressScope{Lineage: scope.Lineage, ContractDigest: strings.Repeat("d", 64), BaseRef: scope.BaseRef}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, seed := progressLease(t, m, repo, tc.taskID, tc.role, tc.name, tc.scope)
			if strings.Contains(seed, "provisional-lock-concern") {
				t.Fatal("foreign progress reused")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "source.txt")
	git(t, repo, "commit", "-m", "changed input")
	_, seed := progressLease(t, m, repo, "task", "review", "changed", scope)
	if strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("changed source inherited progress")
	}
}

func TestOlderReviewCannotOverwriteNewerProgress(t *testing.T) {
	repo := initRepo(t)
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	scope := progressScope()
	one, _ := progressLease(t, m, repo, "task", "review", "one", scope)
	two, _ := progressLease(t, m, repo, "task", "review", "two", scope)
	two.CreatedAt = one.CreatedAt.Add(time.Second)
	newer := strings.ReplaceAll(checkpoint, "provisional-lock-concern", "newer-finding")
	if err := m.CaptureProgress(t.Context(), two, "two", "review", []string{newer}, false); err != nil {
		t.Fatal(err)
	}
	if err := m.CaptureProgress(t.Context(), one, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	_, seed := progressLease(t, m, repo, "task", "review", "three", scope)
	if !strings.Contains(seed, "newer-finding") || strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("late old completion overwrote newer review")
	}
}

func TestMalformedProgressNeverBecomesFinalArtifact(t *testing.T) {
	repo := initRepo(t)
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	lease, _ := progressLease(t, m, repo, "task", "review", "one", progressScope())
	for _, bad := range []string{reviewprogress.Start + `{"verdict":"CLEAN"}` + reviewprogress.End, reviewprogress.Start + strings.Repeat("x", reviewprogress.MaxBytes+1) + reviewprogress.End} {
		if err := m.CaptureProgress(t.Context(), lease, "one", "review", []string{bad}, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(m.progressPath(*lease.ProgressInput)); !os.IsNotExist(err) {
		t.Fatal("invalid checkpoint persisted")
	}
	if err := m.CaptureProgress(t.Context(), lease, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, lease.WorkspaceDir, lease.ScratchDir} {
		if _, err := os.Stat(filepath.Join(dir, ".sybra-review-task.md")); !os.IsNotExist(err) {
			t.Fatal("checkpoint minted final verdict artifact")
		}
	}
}

func TestReviewProgressSurvivesRealCleanRetry(t *testing.T) {
	repo := initRepo(t)
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	one, _ := progressLease(t, m, repo, "task", "review", "one", progressScope())
	if err := m.CaptureProgress(t.Context(), one, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	m.Release(one)
	if err := project.ResetWorktreeForRetry(t.Context(), repo, one.SourceSHA); err != nil {
		t.Fatal(err)
	}
	two, seed := progressLease(t, m, repo, "task", "review", "two", progressScope())
	if two.SourceRefState == one.SourceRefState || !strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("no-op watchdog reset lost unchanged review progress")
	}
}

func TestReviewProgressPinsAuthoritativeTrackingBase(t *testing.T) {
	repo := initRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	git(t, repo, "commit", "--allow-empty", "-m", "fetched main")
	base := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	git(t, repo, "reset", "--hard", head)
	git(t, repo, "update-ref", "refs/remotes/origin/main", base)
	scope := progressScope()
	scope.BaseRef = "refs/remotes/origin/main"
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	one, _ := progressLease(t, m, repo, "task", "review", "one", scope)
	if one.ProgressInput.BaseSHA != base || strings.TrimSpace(git(t, one.WorkspaceDir, "rev-parse", scope.BaseRef)) != base {
		t.Fatal("review base came from stale local main")
	}
	if err := m.CaptureProgress(t.Context(), one, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "update-ref", scope.BaseRef, head)
	_, seed := progressLease(t, m, repo, "task", "review", "two", scope)
	if strings.Contains(seed, "provisional-lock-concern") {
		t.Fatal("base-only movement reused stale notes")
	}
}

func TestReviewProgressRejectsMutatedDisposableInput(t *testing.T) {
	for _, mutation := range []string{"tracked", "commit", "untracked", "base"} {
		t.Run(mutation, func(t *testing.T) {
			repo := initRepo(t)
			head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
			git(t, repo, "update-ref", "refs/remotes/origin/main", head)
			m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
			scope := progressScope()
			scope.BaseRef = "refs/remotes/origin/main"
			lease, _ := progressLease(t, m, repo, "task", "review", "one", scope)
			if mutation == "tracked" || mutation == "untracked" {
				name := "source.txt"
				if mutation == "untracked" {
					name = "injected.txt"
				}
				if err := os.WriteFile(filepath.Join(lease.WorkspaceDir, name), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				git(t, lease.WorkspaceDir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "private edit")
				if mutation == "base" {
					git(t, lease.WorkspaceDir, "update-ref", scope.BaseRef, "HEAD")
					git(t, lease.WorkspaceDir, "reset", "--hard", head)
				}
			}
			if err := m.Finalize(t.Context(), lease, nil, "", ""); err != nil {
				t.Fatal("disposable edits must remain supported:", err)
			}
			if err := m.CaptureProgress(t.Context(), lease, "one", "review", []string{checkpoint}, false); err == nil {
				t.Fatal("modified inspected input produced reusable notes")
			}
		})
	}
}

func TestReviewProgressCorruptCacheStartsFresh(t *testing.T) {
	repo := initRepo(t)
	m := New(filepath.Join(t.TempDir(), "verification"), nil, nil)
	one, _ := progressLease(t, m, repo, "task", "review", "one", progressScope())
	if err := m.CaptureProgress(t.Context(), one, "one", "review", []string{checkpoint}, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{", strings.Repeat("x", 32<<10)} {
		if err := os.WriteFile(m.progressPath(*one.ProgressInput), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		_, seed := progressLease(t, m, repo, "task", "review", "fresh", progressScope())
		if strings.Contains(seed, "provisional-lock-concern") {
			t.Fatal("corrupt notes reused")
		}
	}
}
