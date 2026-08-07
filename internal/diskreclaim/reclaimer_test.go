package diskreclaim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// TestMain fails the whole package immediately if git is missing, rather
// than letting individual tests silently t.Skip — a stripped-down test
// environment should show up as a red CI run, not quietly reduced coverage.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/diskreclaim tests require git on PATH:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type fakeLister struct {
	tasks []task.Task
	err   error
}

func (f *fakeLister) List() ([]task.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]task.Task, len(f.tasks))
	copy(out, f.tasks)
	return out, nil
}

// testConfig returns a Config rooted at a fresh temp dir, with SYBRA_HOME
// pointed there too — internal/cleanup's sandbox/go-build-cache/worktree
// bucket roots key off config.HomeDir() directly, not a Config field.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Logging.Dir = filepath.Join(home, "logs")
	cfg.WorktreesDir = filepath.Join(home, "worktrees")
	cfg.Audit.RetentionDays = 30
	cfg.Agent.LogRetentionDays = 14
	return cfg
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
		panic("unreachable")
	}
}

func writeFileAt(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
		panic("unreachable")
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
		panic("unreachable")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(scrubGitFixtureEnv(os.Environ()),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.local",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.local",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
		panic("unreachable")
	}
}

func scrubGitFixtureEnv(env []string) []string {
	return withoutEnvKeys(env,
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_COMMON_DIR",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	)
}

func withoutEnvKeys(env []string, keys ...string) []string {
	if len(env) == 0 || len(keys) == 0 {
		return env
	}
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, skip := blocked[name]; skip {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

// makeCleanGitWorktree creates a real, initialized, clean git repo at path,
// pushed to a throwaway bare "origin" so it passes both cleanup's
// dirty-worktree and unpushed-commits safety checks — matching production,
// where every Sybra-managed worktree tracks the project's bare clone as
// origin.
func makeCleanGitWorktree(t *testing.T, path string) {
	t.Helper()
	mustMkdir(t, path)
	runGit(t, path, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
		panic("unreachable")
	}
	runGit(t, path, "add", "-A")
	runGit(t, path, "commit", "-q", "-m", "init")

	origin := t.TempDir()
	runGit(t, origin, "init", "-q", "--bare", "-b", "main")
	runGit(t, path, "remote", "add", "origin", origin)
	runGit(t, path, "push", "-q", "-u", "origin", "main")
	runGit(t, path, "fetch", "-q", "origin")
}

func TestReclaimerRunReclaimsSafeBucketsOnly(t *testing.T) {
	cfg := testConfig(t)
	old := time.Now().Add(-60 * 24 * time.Hour)

	writeFileAt(t, filepath.Join(cfg.Logging.Dir, "agents", "old.ndjson"), 1024, old)
	writeFileAt(t, filepath.Join(cfg.AuditDir(), "old.ndjson"), 2048, old)

	// A destructive-bucket resource (an orphaned, clean worktree) must never
	// be deleted by an automatic reclaim pass.
	worktreePath := filepath.Join(cfg.WorktreesDir, "abc12345")
	makeCleanGitWorktree(t, worktreePath)

	r := New(cfg, &fakeLister{}, time.Minute, nil)
	r.run()

	outcome := r.LastOutcome()
	if outcome.ReclaimedBytes != 1024+2048 {
		t.Fatalf("ReclaimedBytes = %d, want %d", outcome.ReclaimedBytes, 1024+2048)
	}
	if outcome.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", outcome.Errors)
	}
	if outcome.RanAt.IsZero() {
		t.Fatal("RanAt is zero, want a timestamp")
	}

	if _, err := os.Stat(filepath.Join(cfg.Logging.Dir, "agents", "old.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("old agent log was not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.AuditDir(), "old.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("old audit log was not removed (err=%v)", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree must never be auto-deleted, but stat failed: %v", err)
		panic("unreachable")
	}
}

func TestReclaimerRunReportsUnreclaimableDestructiveBucketSize(t *testing.T) {
	cfg := testConfig(t)
	worktreePath := filepath.Join(cfg.WorktreesDir, "abc12345")
	makeCleanGitWorktree(t, worktreePath)

	r := New(cfg, &fakeLister{}, time.Minute, nil)
	r.run()

	outcome := r.LastOutcome()
	if outcome.UnreclaimableBytes <= 0 {
		t.Fatalf("UnreclaimableBytes = %d, want > 0 (orphaned clean worktree should be sized, not deleted)", outcome.UnreclaimableBytes)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree must still exist after a report-only scan: %v", err)
		panic("unreachable")
	}
}

func TestReclaimerRunProtectsActiveTaskResources(t *testing.T) {
	cfg := testConfig(t)
	old := time.Now().Add(-60 * 24 * time.Hour)
	writeFileAt(t, filepath.Join(cfg.Logging.Dir, "agents", "old.ndjson"), 512, old)

	sandboxDir := filepath.Join(cfg.Logging.Dir, "..", "sandboxes", "active123")
	writeFileAt(t, filepath.Join(sandboxDir, "f.bin"), 4096, old)

	lister := &fakeLister{tasks: []task.Task{
		{ID: "active123", Status: task.StatusInProgress, StatusChangedAt: time.Now()},
	}}

	r := New(cfg, lister, time.Minute, nil)
	r.run()

	outcome := r.LastOutcome()
	// Only the age-eligible log should be reclaimed; the active task's
	// sandbox must be left alone.
	if outcome.ReclaimedBytes != 512 {
		t.Fatalf("ReclaimedBytes = %d, want 512 (active task's sandbox must not be counted)", outcome.ReclaimedBytes)
	}
	if _, err := os.Stat(filepath.Join(sandboxDir, "f.bin")); err != nil {
		t.Fatalf("active task's sandbox file must not be removed: %v", err)
		panic("unreachable")
	}
}

func TestReclaimerRunIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	old := time.Now().Add(-60 * 24 * time.Hour)
	writeFileAt(t, filepath.Join(cfg.Logging.Dir, "agents", "old.ndjson"), 1024, old)

	r := New(cfg, &fakeLister{}, time.Minute, nil)
	r.run()
	first := r.LastOutcome()
	if first.ReclaimedBytes != 1024 {
		t.Fatalf("first pass ReclaimedBytes = %d, want 1024", first.ReclaimedBytes)
	}

	r.run()
	second := r.LastOutcome()
	if second.ReclaimedBytes != 0 {
		t.Fatalf("second pass ReclaimedBytes = %d, want 0 (already-clean state must be a no-op)", second.ReclaimedBytes)
	}
	if second.Errors != 0 {
		t.Fatalf("second pass Errors = %d, want 0", second.Errors)
	}
}

func TestReclaimerRunCountsScanFailureAsError(t *testing.T) {
	cfg := testConfig(t)
	lister := &fakeLister{err: fmt.Errorf("boom")}

	r := New(cfg, lister, time.Minute, nil)
	r.run()

	outcome := r.LastOutcome()
	if outcome.Errors == 0 {
		t.Fatal("Errors = 0, want > 0 when the safe-bucket scan fails outright")
	}
}

func TestReclaimerTryRunDedupsWhileRunning(t *testing.T) {
	r := New(testConfig(t), &fakeLister{}, time.Minute, nil)
	r.running = true
	if r.TryRun() {
		t.Fatal("TryRun must not start a second pass while one is in flight")
	}
}

func TestReclaimerTryRunRespectsCooldown(t *testing.T) {
	r := New(testConfig(t), &fakeLister{}, time.Hour, nil)
	r.lastRun = time.Now()
	if r.TryRun() {
		t.Fatal("TryRun must not start before the cooldown elapses")
	}
}

func TestReclaimerTryRunStartsWhenIdleAndCooldownElapsed(t *testing.T) {
	r := New(testConfig(t), &fakeLister{}, time.Millisecond, nil)
	// testConfig registered the temp home's cleanup first, and t.Cleanup runs
	// LIFO, so this drains the pass before RemoveAll touches the directories
	// it is still writing into.
	t.Cleanup(r.Wait)
	if !r.TryRun() {
		t.Fatal("TryRun should start a pass when idle and the cooldown has elapsed")
	}
	waitForReclaimerIdle(t, r)
}

func waitForReclaimerIdle(t *testing.T, r *Reclaimer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reclaimer did not finish")
}

// TestReclaimerWaitDrainsInFlightPass pins the guarantee the cleanup above
// depends on: after Wait returns, no pass is still writing. Without it the
// package's temp home is torn down under a live writer, which surfaced as an
// unrelated-looking "directory not empty" cleanup failure.
func TestReclaimerWaitDrainsInFlightPass(t *testing.T) {
	r := New(testConfig(t), &fakeLister{}, time.Millisecond, nil)
	if !r.TryRun() {
		t.Fatal("TryRun should start a pass")
	}
	r.Wait()

	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if running {
		t.Fatal("a pass is still running after Wait returned")
	}
}

// TestReclaimerWaitNilIsSafe mirrors TryRun's nil-receiver contract: callers
// hold a possibly-nil Reclaimer (App.getDiskReclaimer returns nil when the
// app is not fully wired), so Wait must not panic on one.
func TestReclaimerWaitNilIsSafe(t *testing.T) {
	var r *Reclaimer
	r.Wait()
}

func TestReclaimerTryRunNilReclaimerIsSafe(t *testing.T) {
	var r *Reclaimer
	if r.TryRun() {
		t.Fatal("TryRun on a nil Reclaimer must return false")
	}
	if got := r.LastOutcome(); got != (Outcome{}) {
		t.Fatalf("LastOutcome on a nil Reclaimer = %+v, want zero value", got)
	}
}

func TestNewFallsBackToDefaultCooldown(t *testing.T) {
	r := New(testConfig(t), &fakeLister{}, 0, nil)
	if r.cooldown != DefaultCooldown {
		t.Fatalf("cooldown = %v, want %v", r.cooldown, DefaultCooldown)
	}
}
