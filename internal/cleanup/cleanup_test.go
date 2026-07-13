package cleanup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/buildcache"
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
// pointed there too (sandboxesDir/goBuildCacheDir key off config.HomeDir()
// directly, not a Config field).
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

// makeGitWorktree creates a real, initialized git repo at path with one
// committed file. dirty additionally leaves an uncommitted modification.
func makeGitWorktree(t *testing.T, path string, dirty bool) {
	t.Helper()
	mustMkdir(t, path)
	runGit(t, path, "init", "-q")
	if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, path, "add", "-A")
	runGit(t, path, "commit", "-q", "-m", "init")
	if dirty {
		if err := os.WriteFile(filepath.Join(path, "f.txt"), []byte("b"), 0o644); err != nil {
			t.Fatalf("dirty file: %v", err)
		}
	}
}

func doneTask(id string, statusChangedAt time.Time) task.Task {
	return task.Task{ID: id, Status: task.StatusDone, StatusChangedAt: statusChangedAt}
}

// --- eligible() -------------------------------------------------------

func TestEligibleActiveTaskSkipped(t *testing.T) {
	snap := snapshot{byID: map[string]task.Task{
		"abc12345": {ID: "abc12345", Status: task.StatusInProgress, StatusChangedAt: time.Now().Add(-48 * time.Hour)},
	}}
	ok, reason := eligible(snap, "abc12345", "/some/path", time.Hour, false, time.Now())
	if ok {
		t.Fatalf("active task must not be eligible, got reason %q", reason)
	}
}

func TestEligibleOrphanIsEligible(t *testing.T) {
	snap := snapshot{byID: map[string]task.Task{}}
	ok, _ := eligible(snap, "missing1", "/some/path", time.Hour, false, time.Now())
	if !ok {
		t.Fatal("orphan (task not in store) must be eligible")
	}
}

func TestEligibleTerminalStatusesPastRetention(t *testing.T) {
	now := time.Now()
	for _, st := range []task.Status{task.StatusDone, task.StatusCancelled, task.StatusBlocked} {
		snap := snapshot{byID: map[string]task.Task{
			"t": {ID: "t", Status: st, StatusChangedAt: now.Add(-2 * time.Hour)},
		}}
		ok, reason := eligible(snap, "t", "/p", time.Hour, false, now)
		if !ok {
			t.Fatalf("status %s past retention must be eligible, got reason %q", st, reason)
		}
	}
}

func TestEligibleRetentionBoundary(t *testing.T) {
	now := time.Now()
	retention := time.Hour
	snap := snapshot{byID: map[string]task.Task{
		"within": {ID: "within", Status: task.StatusDone, StatusChangedAt: now.Add(-30 * time.Minute)},
		"past":   {ID: "past", Status: task.StatusDone, StatusChangedAt: now.Add(-90 * time.Minute)},
	}}
	if ok, _ := eligible(snap, "within", "/p", retention, false, now); ok {
		t.Fatal("within retention window must not be eligible")
	}
	if ok, _ := eligible(snap, "past", "/p", retention, false, now); !ok {
		t.Fatal("past retention window must be eligible")
	}
}

func TestEligibleRetentionDisabled(t *testing.T) {
	now := time.Now()
	snap := snapshot{byID: map[string]task.Task{
		"t": {ID: "t", Status: task.StatusDone, StatusChangedAt: now.Add(-1000 * time.Hour)},
	}}
	if ok, _ := eligible(snap, "t", "/p", time.Hour, true, now); ok {
		t.Fatal("retention disabled must never be eligible via age")
	}
}

func TestEligibleUnknownTaskIDNeverEligible(t *testing.T) {
	snap := snapshot{byID: map[string]task.Task{}}
	if ok, _ := eligible(snap, unknownTaskID, "/p", time.Hour, false, time.Now()); ok {
		t.Fatal("unknown task id must never be eligible")
	}
	if ok, _ := eligible(snap, "", "/p", time.Hour, false, time.Now()); ok {
		t.Fatal("empty task id must never be eligible")
	}
}

// --- helpers ------------------------------------------------------------

func TestTaskIDFromWorktreeDir(t *testing.T) {
	cases := map[string]string{
		"f581145e":             "f581145e",
		"my-feature-f581145e":  "f581145e",
		"a-b-c-deadbeef":       "deadbeef",
		"not-an-id":            unknownTaskID,
		"deadbee":              unknownTaskID, // 7 hex chars, too short
		"deadbeefg":            unknownTaskID, // 'g' not hex
		"UPPERCASE-DEADBEEF12": unknownTaskID, // uppercase not matched
	}
	for name, want := range cases {
		if got := taskIDFromWorktreeDir(name); got != want {
			t.Errorf("taskIDFromWorktreeDir(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestContainedChild(t *testing.T) {
	root := "/home/sybra/worktrees"
	cases := []struct {
		path string
		ok   bool
	}{
		{"/home/sybra/worktrees/task-abc12345", true},
		{"/home/sybra/worktrees", false},              // root itself, not a child
		{"/home/sybra/worktrees/../other", false},      // escapes
		{"/home/sybra/worktreesXYZ/task", false},       // sibling prefix collision
		{"/etc/passwd", false},                         // unrelated absolute path
		{"/home/sybra/worktrees/a/../../etc", false},   // escapes via traversal
	}
	for _, c := range cases {
		_, ok := containedChild(root, c.path)
		if ok != c.ok {
			t.Errorf("containedChild(%q, %q) ok = %v, want %v", root, c.path, ok, c.ok)
		}
	}
}

func TestIsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	mustMkdir(t, real)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if !isSymlink(link) {
		t.Error("expected link to be reported as a symlink")
	}
	if isSymlink(real) {
		t.Error("expected real dir not to be reported as a symlink")
	}
	if isSymlink(filepath.Join(dir, "missing")) {
		t.Error("expected missing path not to be reported as a symlink")
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "a"), 100, time.Now())
	writeFileAt(t, filepath.Join(dir, "sub", "b"), 250, time.Now())
	size, err := dirSize(dir)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if size != 350 {
		t.Fatalf("dirSize = %d, want 350", size)
	}
}

// --- Scan: logs/audit age eligibility -----------------------------------

func TestScanLogsOlderThanOverride(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agent.LogRetentionDays = 365 // config default would keep everything

	now := time.Now()
	agentsDir := filepath.Join(cfg.Logging.Dir, "agents")
	oldLog := filepath.Join(agentsDir, "agent-old.ndjson")
	newLog := filepath.Join(agentsDir, "agent-new.ndjson")
	writeFileAt(t, oldLog, 10, now.Add(-72*time.Hour))
	writeFileAt(t, newLog, 10, now.Add(-1*time.Hour))

	s := NewScanner(cfg, &fakeLister{})
	// Without an override, config retention (365d) keeps both.
	res, err := s.Scan(Options{Only: []string{BucketLogs}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := res.Buckets[0].Items; got != 0 {
		t.Fatalf("expected 0 eligible logs under the config default, got %d", got)
	}

	// --older-than overrides the log/audit threshold specifically.
	res, err = s.Scan(Options{Only: []string{BucketLogs}, OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := res.Buckets[0].Items; got != 1 {
		t.Fatalf("expected 1 eligible log with --older-than 24h, got %d", got)
	}
	if res.Buckets[0].Paths[0] != oldLog {
		t.Fatalf("expected old log to be the eligible one, got %v", res.Buckets[0].Paths)
	}
}

func TestScanAuditUsesOwnRetentionNotOverriddenByDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.Audit.RetentionDays = 7

	now := time.Now()
	auditDir := cfg.AuditDir()
	oldAudit := filepath.Join(auditDir, "2020-01-01.ndjson")
	newAudit := filepath.Join(auditDir, "2020-01-09.ndjson")
	writeFileAt(t, oldAudit, 20, now.Add(-10*24*time.Hour))
	writeFileAt(t, newAudit, 20, now.Add(-1*24*time.Hour))

	s := NewScanner(cfg, &fakeLister{})
	res, err := s.Scan(Options{Only: []string{BucketAudit}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	b := res.Buckets[0]
	if b.Items != 1 || b.Paths[0] != oldAudit {
		t.Fatalf("expected only the old audit file eligible, got %+v", b)
	}
}

// --- Scan: sandboxes / go-build-cache ------------------------------------

func TestScanSandboxesSkipsActiveOrphanAndTerminal(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1

	now := time.Now()
	sbDir := sandboxesDir()
	active := filepath.Join(sbDir, "active1234")
	orphan := filepath.Join(sbDir, "orphan123")
	terminal := filepath.Join(sbDir, "term12345")
	writeFileAt(t, filepath.Join(active, "f"), 5, now)
	writeFileAt(t, filepath.Join(orphan, "f"), 7, now)
	writeFileAt(t, filepath.Join(terminal, "f"), 11, now)

	lister := &fakeLister{tasks: []task.Task{
		{ID: "active1234", Status: task.StatusInProgress, StatusChangedAt: now},
		doneTask("term12345", now.Add(-2*time.Hour)),
		// "orphan123" intentionally absent from the store.
	}}

	s := NewScanner(cfg, lister)
	res, err := s.Scan(Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	b := res.Buckets[0]
	if b.Items != 2 {
		t.Fatalf("expected 2 eligible sandboxes (orphan + terminal past retention), got %d: %v", b.Items, b.Paths)
	}
	if b.Bytes != 18 {
		t.Fatalf("expected 18 bytes total, got %d", b.Bytes)
	}
	for _, p := range b.Paths {
		if p == active {
			t.Fatalf("active task's sandbox must not be reported: %v", b.Paths)
		}
	}
}

func TestScanGoBuildCacheOrphanEligible(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1
	now := time.Now()

	orphanDir := filepath.Join(goBuildCacheDir(), "orphan999")
	writeFileAt(t, filepath.Join(orphanDir, "cache.a"), 42, now)

	s := NewScanner(cfg, &fakeLister{})
	res, err := s.Scan(Options{Only: []string{BucketGoBuildCache}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	b := res.Buckets[0]
	if b.Items != 1 || b.Bytes != 42 {
		t.Fatalf("expected 1 orphaned go-build-cache dir sized 42, got %+v", b)
	}
}

// --- Scan: worktrees ------------------------------------------------------

func TestScanWorktreesRequiresGateFlag(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	wtPath := filepath.Join(cfg.WorktreesDir, "d00e1234")
	makeGitWorktree(t, wtPath, false)

	lister := &fakeLister{tasks: []task.Task{doneTask("d00e1234", now.Add(-100*time.Hour))}}
	s := NewScanner(cfg, lister)

	res, err := s.Scan(Options{Only: []string{BucketWorktrees}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Buckets) != 0 {
		t.Fatalf("worktrees bucket must be absent without --worktrees, got %+v", res.Buckets)
	}

	res, err = s.Scan(Options{Only: []string{BucketWorktrees}, Worktrees: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Buckets) != 1 || res.Buckets[0].Items != 1 {
		t.Fatalf("expected 1 eligible worktree with --worktrees, got %+v", res.Buckets)
	}
	if res.Buckets[0].Risk != RiskDestructive {
		t.Fatalf("worktrees bucket must be RiskDestructive, got %v", res.Buckets[0].Risk)
	}
}

func TestScanWorktreesDirtySkippedWithoutForce(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	dirtyPath := filepath.Join(cfg.WorktreesDir, "1234dead")
	makeGitWorktree(t, dirtyPath, true)

	lister := &fakeLister{tasks: []task.Task{doneTask("1234dead", now.Add(-100*time.Hour))}}
	s := NewScanner(cfg, lister)

	res, err := s.Scan(Options{Only: []string{BucketWorktrees}, Worktrees: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Buckets[0].Items != 0 {
		t.Fatalf("dirty worktree must not be reported eligible without --force, got %+v", res.Buckets[0])
	}

	res, err = s.Scan(Options{Only: []string{BucketWorktrees}, Worktrees: true, Force: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Buckets[0].Items != 1 {
		t.Fatalf("dirty worktree must be reported eligible with --force, got %+v", res.Buckets[0])
	}
}

func TestScanWorktreesUnknownNameNeverEligible(t *testing.T) {
	cfg := testConfig(t)
	unknownPath := filepath.Join(cfg.WorktreesDir, "hand-created-checkout")
	makeGitWorktree(t, unknownPath, false)

	s := NewScanner(cfg, &fakeLister{})
	res, err := s.Scan(Options{Only: []string{BucketWorktrees}, Worktrees: true, Force: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Buckets[0].Items != 0 {
		t.Fatalf("unknown-named worktree dir must never be eligible, even with --force: %+v", res.Buckets[0])
	}
}

func TestScanSymlinkedSandboxRefused(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	sbDir := sandboxesDir()
	mustMkdir(t, sbDir)
	link := filepath.Join(sbDir, "orphanlink")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s := NewScanner(cfg, &fakeLister{})
	res, err := s.Scan(Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Buckets[0].Items != 0 {
		t.Fatalf("symlinked sandbox dir must never be reported eligible: %+v", res.Buckets[0])
	}
	if _, err := os.Lstat(outsideDir); err != nil {
		t.Fatalf("outside dir must be untouched: %v", err)
	}
}

// --- Scan: shared-cache / external ----------------------------------------

func TestScanSharedCacheRequiresForce(t *testing.T) {
	cfg := testConfig(t)
	writeFileAt(t, filepath.Join(buildcache.SharedGoModDir(), "x"), 100, time.Now())

	s := NewScanner(cfg, &fakeLister{})
	res, err := s.Scan(Options{Only: []string{BucketSharedCache}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Buckets) != 0 {
		t.Fatalf("shared-cache must be absent without --force, got %+v", res.Buckets)
	}

	res, err = s.Scan(Options{Only: []string{BucketSharedCache}, Force: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Buckets) != 1 || res.Buckets[0].Bytes != 100 {
		t.Fatalf("expected shared-cache bucket with 100 bytes given --force, got %+v", res.Buckets)
	}
	if res.Buckets[0].Risk != RiskDestructive {
		t.Fatalf("shared-cache must be RiskDestructive, got %v", res.Buckets[0].Risk)
	}
}

func TestScanExternalIsReportOnlyAndNeverErrors(t *testing.T) {
	cfg := testConfig(t)
	s := NewScanner(cfg, &fakeLister{})
	s.externalRunner = func() (string, error) { return "Images  5  2.1GB reclaimable", nil }

	res, err := s.Scan(Options{Only: []string{BucketExternal}, External: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Buckets) != 1 {
		t.Fatalf("expected external bucket present with --external, got %+v", res.Buckets)
	}
	if len(res.Buckets[0].Paths) != 0 {
		t.Fatalf("external bucket must never carry real filesystem paths, got %v", res.Buckets[0].Paths)
	}
}

func TestValidateOnlyRejectsUnknownBucket(t *testing.T) {
	cfg := testConfig(t)
	s := NewScanner(cfg, &fakeLister{})
	if _, err := s.Scan(Options{Only: []string{"not-a-bucket"}}); err == nil {
		t.Fatal("expected an error for an unknown bucket name")
	}
}

// --- Apply ------------------------------------------------------------

func TestApplyDeletesOnlySafeBucketsSelected(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1
	now := time.Now()

	orphanDir := filepath.Join(sandboxesDir(), "orphan123")
	writeFileAt(t, filepath.Join(orphanDir, "f"), 10, now)

	lister := &fakeLister{}
	s := NewScanner(cfg, lister)
	scan, err := s.Scan(Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	applyRes, err := s.Apply(scan.Buckets, Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRes.Buckets[0].Removed != 1 || applyRes.Buckets[0].ReclaimedBytes != 10 {
		t.Fatalf("expected 1 removed / 10 bytes reclaimed, got %+v", applyRes.Buckets[0])
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("expected orphan sandbox dir to be removed, stat err = %v", err)
	}
}

func TestApplyRevalidatesAgainstFreshSnapshotRace(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1
	now := time.Now()

	dir := filepath.Join(sandboxesDir(), "raceme12")
	writeFileAt(t, filepath.Join(dir, "f"), 10, now)

	lister := &fakeLister{} // orphan at scan time
	s := NewScanner(cfg, lister)
	scan, err := s.Scan(Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Buckets[0].Items != 1 {
		t.Fatalf("expected orphan sandbox reported eligible at scan time, got %+v", scan.Buckets[0])
	}

	// Task reappears (re-activated) between Scan and Apply.
	lister.tasks = []task.Task{{ID: "raceme12", Status: task.StatusInProgress, StatusChangedAt: now}}

	applyRes, err := s.Apply(scan.Buckets, Options{Only: []string{BucketSandboxes}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRes.Buckets[0].Removed != 0 {
		t.Fatalf("expected 0 removed once the task became active again, got %+v", applyRes.Buckets[0])
	}
	if len(applyRes.Buckets[0].Skipped) != 1 {
		t.Fatalf("expected the path to be recorded as skipped, got %+v", applyRes.Buckets[0])
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected sandbox dir to survive the race, stat err = %v", err)
	}
}

func TestApplyExternalBucketNeverDeletes(t *testing.T) {
	cfg := testConfig(t)
	s := NewScanner(cfg, &fakeLister{})
	fakeBucket := Bucket{Name: BucketExternal, Risk: RiskCaution, Paths: []string{"docker"}}

	res, err := s.Apply([]Bucket{fakeBucket}, Options{External: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Buckets[0].Removed != 0 || len(res.Buckets[0].Skipped) != 1 {
		t.Fatalf("expected external bucket entirely skipped, got %+v", res.Buckets[0])
	}
}

func TestApplyOutOfRootPathRefused(t *testing.T) {
	cfg := testConfig(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := NewScanner(cfg, &fakeLister{})
	// A hand-crafted bucket whose Paths escape the bucket's real root —
	// simulates a corrupted/forged scan result.
	fakeBucket := Bucket{Name: BucketSandboxes, Risk: RiskSafe, Paths: []string{secret}}
	res, err := s.Apply([]Bucket{fakeBucket}, Options{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Buckets[0].Removed != 0 {
		t.Fatalf("expected out-of-root path to be refused, got %+v", res.Buckets[0])
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("expected out-of-root file to survive: %v", err)
	}
}

// --- Acceptance: --apply --force --worktrees survivors --------------------

func TestApplyForceWorktreesSurvivesActiveTask(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sandbox.RetentionHours = 1
	now := time.Now()

	activeSandbox := filepath.Join(sandboxesDir(), "acedface")
	writeFileAt(t, filepath.Join(activeSandbox, "f"), 5, now)
	activeGoBuild := filepath.Join(goBuildCacheDir(), "acedface")
	writeFileAt(t, filepath.Join(activeGoBuild, "f"), 5, now)
	activeWorktree := filepath.Join(cfg.WorktreesDir, "acedface")
	makeGitWorktree(t, activeWorktree, true) // dirty, but irrelevant: task is active

	doneWorktree := filepath.Join(cfg.WorktreesDir, "d00d5678")
	makeGitWorktree(t, doneWorktree, false)

	lister := &fakeLister{tasks: []task.Task{
		{ID: "acedface", Status: task.StatusInProgress, StatusChangedAt: now},
		doneTask("d00d5678", now.Add(-100*time.Hour)),
	}}
	s := NewScanner(cfg, lister)

	opts := Options{Worktrees: true, Force: true}
	scan, err := s.Scan(opts)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	applyRes, err := s.Apply(scan.Buckets, opts)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	_ = applyRes

	for _, p := range []string{activeSandbox, activeGoBuild, activeWorktree} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("active task resource must survive --apply --force --worktrees: %s: %v", p, err)
		}
	}
	if _, err := os.Stat(doneWorktree); !os.IsNotExist(err) {
		t.Fatalf("done task's worktree should have been removed, stat err = %v", err)
	}
}
