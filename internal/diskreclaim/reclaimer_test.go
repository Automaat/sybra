package diskreclaim

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

type fakeLister struct {
	tasks []task.Task
}

func (f *fakeLister) List() ([]task.Task, error) {
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
	}
}

func writeFileAt(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.local",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.local",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeCleanGitWorktree creates a real, initialized, clean git repo at path
// so it passes cleanup's dirty-worktree safety check.
func makeCleanGitWorktree(t *testing.T, path string) {
	t.Helper()
	mustMkdir(t, path)
	runGit(t, path, "init", "-q")
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, path, "add", "-A")
	runGit(t, path, "commit", "-q", "-m", "init")
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
	if !r.TryRun() {
		t.Fatal("TryRun should start a pass when idle and the cooldown has elapsed")
	}
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
