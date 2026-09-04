package cleanup

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
)

func newProtectedStore(t *testing.T) *ProtectedStore {
	t.Helper()
	store := NewProtectedStore(filepath.Join(t.TempDir(), "protected-findings.json"))
	store.clock = clock.NewFake(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	store.reminderWindow = time.Hour
	return store
}

func TestProtectedStoreObserveDeduplicatesWithinReminderWindow(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}

	first, event, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(first): %v", err)
	}
	if event != ObserveCreated {
		t.Fatalf("first event = %q, want %q", event, ObserveCreated)
	}

	second, event, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(second): %v", err)
	}
	if event != ObserveUnchanged {
		t.Fatalf("second event = %q, want %q", event, ObserveUnchanged)
	}
	if second.LastLoggedAt != first.LastLoggedAt {
		t.Fatalf("LastLoggedAt changed unexpectedly: first=%s second=%s", first.LastLoggedAt, second.LastLoggedAt)
	}
}

func TestProtectedStoreObserveStateChangeReopensDiscardedFinding(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}
	first, _, err := store.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(first): %v", err)
	}
	if _, err := store.Discard(first.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	store.clock = clock.NewFake(time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC))
	next := obs
	next.ObservedHead = "def456"
	next.ObservedState = "dirty=true"
	got, event, err := store.Observe(next)
	if err != nil {
		t.Fatalf("Observe(reopen): %v", err)
	}
	if event != ObserveReopened {
		t.Fatalf("event = %q, want %q", event, ObserveReopened)
	}
	if got.State != FindingOpen {
		t.Fatalf("State = %q, want %q", got.State, FindingOpen)
	}
	if got.ObservedHead != "def456" || got.ObservedState != "dirty=true" {
		t.Fatalf("updated finding = %+v", got)
	}
}

func TestProtectedStoreIndependentStoresPreserveResolvedState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "protected-findings.json")
	firstStore := NewProtectedStore(path)
	secondStore := NewProtectedStore(path)
	fake := clock.NewFake(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	firstStore.clock = fake
	secondStore.clock = fake
	obs := Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}

	first, _, err := firstStore.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(first): %v", err)
	}
	if _, err := secondStore.Discard(first.ID); err != nil {
		t.Fatalf("Discard(second store): %v", err)
	}
	got, event, err := firstStore.Observe(obs)
	if err != nil {
		t.Fatalf("Observe(after discard): %v", err)
	}
	if event != ObserveUnchanged {
		t.Fatalf("event = %q, want %q", event, ObserveUnchanged)
	}
	if got.State != FindingDiscarded {
		t.Fatalf("State = %q, want %q", got.State, FindingDiscarded)
	}
}

func TestProtectedStoreSeparateProcessWaitsForStoreLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "protected-findings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unlock, err := fsutil.LockFile(path)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestProtectedStoreHelperProcess", "--", path)
	cmd.Env = append(os.Environ(), "SYBRA_PROTECTED_STORE_HELPER=observe")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		_ = unlock()
		t.Fatalf("start helper: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		_ = unlock()
		t.Fatalf("helper exited while parent held lock: err=%v output=%s", err, output.String())
	case <-time.After(200 * time.Millisecond):
	}

	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = unlock()
		t.Fatalf("helper wrote store while lock was held: %s", data)
	}

	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("helper after unlock: %v output=%s", err, output.String())
		}
	case <-ctx.Done():
		t.Fatalf("helper did not finish after unlock: %v output=%s", ctx.Err(), output.String())
	}

	store := NewProtectedStore(path)
	findings, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(findings))
	}
}

func TestProtectedStoreHelperProcess(t *testing.T) {
	if os.Getenv("SYBRA_PROTECTED_STORE_HELPER") != "observe" {
		return
	}
	if len(os.Args) == 0 {
		t.Fatal("missing argv")
	}
	path := os.Args[len(os.Args)-1]
	store := NewProtectedStore(path)
	store.clock = clock.NewFake(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC))
	if _, _, err := store.Observe(Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-helper",
		Path:          "/tmp/worktree-helper",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 128,
	}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
}

func TestProtectedStoreRescueFailureLeavesFindingOpen(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	dir := filepath.Join(t.TempDir(), "not-a-git-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, _, err := store.Observe(Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-1",
		Path:          dir,
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 64,
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if _, err := store.Rescue(got.ID); err == nil {
		t.Fatal("Rescue() error = nil, want git rescue failure")
	}
	reloaded, ok, err := store.Get(got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get() ok = false, want finding to remain present")
	}
	if reloaded.State != FindingOpen {
		t.Fatalf("State after failed rescue = %q, want %q", reloaded.State, FindingOpen)
	}
}

func TestRescueWorktreeCreatesRefAndBundle(t *testing.T) {
	worktree := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = worktree
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	ref := "refs/sybra/rescue/test"
	bundle := filepath.Join(t.TempDir(), "rescue.bundle")
	if err := rescueWorktree(worktree, ref, bundle); err != nil {
		t.Fatalf("rescueWorktree: %v", err)
	}
	if info, err := os.Stat(bundle); err != nil || info.Size() == 0 {
		t.Fatalf("bundle stat: info=%v err=%v", info, err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", ref)
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("verify rescue ref: %v: %s", err, out)
	}
}

func TestProtectedEvidenceLogPathsIncludesReattachedFinding(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	got, _, err := store.Observe(Observation{
		Kind:          ResourceWorktree,
		TaskID:        "task-old",
		Path:          "/tmp/worktree-1",
		Reason:        ReasonUnpushedCommits,
		ObservedHead:  "abc123",
		ObservedState: "dirty=false",
		BytesRetained: 64,
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	reattached, err := store.Reattach(got.ID, "task-new")
	if err != nil {
		t.Fatalf("Reattach: %v", err)
	}

	logDir := t.TempDir()
	paths := ProtectedEvidenceLogPaths(logDir, []task.Task{
		{
			ID: "task-old",
			AgentRuns: []task.AgentRun{
				{LogFile: "old.ndjson"},
			},
		},
		{
			ID: "task-new",
			AgentRuns: []task.AgentRun{
				{LogFile: "new.ndjson"},
			},
		},
	}, []Finding{reattached})

	if !paths[filepath.Join(logDir, "worktrees", "task-new-setup.log")] {
		t.Fatal("reattached setup log was not protected")
	}
	if !paths[filepath.Join(logDir, "agents", "new.ndjson")] {
		t.Fatal("reattached agent log was not protected")
	}
	if paths[filepath.Join(logDir, "agents", "old.ndjson")] {
		t.Fatal("original task agent log protected after reattach")
	}
}

func TestArchiveDirectoryReturnsFlushError(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte(strings.Repeat("x", 1024)), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	if err := archiveDirectory(root, "/dev/full"); err == nil {
		t.Fatal("archiveDirectory() error = nil, want flush error")
	}
}

func TestProtectedStoreObserveConcurrentScansDeduplicates(t *testing.T) {
	t.Parallel()
	store := newProtectedStore(t)
	obs := Observation{
		Kind:          ResourceSandbox,
		TaskID:        "task-2",
		Path:          "/tmp/sandbox-2",
		Reason:        ReasonUnpushedCommits,
		ObservedState: "bytes=32",
		BytesRetained: 32,
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := store.Observe(obs); err != nil {
				t.Errorf("Observe: %v", err)
			}
		})
	}
	wg.Wait()

	findings, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(findings))
	}
}
