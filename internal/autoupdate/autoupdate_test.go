package autoupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckAndApplyAutoModeFastForwards(t *testing.T) {
	ctx := t.Context()
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:      true,
		RepoDir:      work,
		Remote:       "origin",
		Branch:       "main",
		Mode:         ModeAuto,
		PollInterval: time.Hour,
	}, nil)

	res, err := r.CheckAndApply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "applied" {
		t.Fatalf("status = %q, want applied (reason=%q)", res.Status, res.Reason)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); err != nil {
		t.Fatalf("feature.txt missing after auto mode: %v", err)
	}
}

func TestRunRequestsRestartAfterAutoApply(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	restarted := false
	r := New(Config{
		Enabled: true,
		RepoDir: work,
		Remote:  "origin",
		Branch:  "main",
		Mode:    ModeAuto,
		RequestRestart: func() {
			restarted = true
		},
	}, nil)

	r.check(t.Context())
	if !restarted {
		t.Fatal("restart was not requested after auto apply")
	}
}

func TestWriteRestartMarkerCreatesMarker(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "nested", "sybra-home")

	if err := WriteRestartMarker(homeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(RestartMarkerPath(homeDir)); err != nil {
		t.Fatalf("restart marker missing: %v", err)
	}
}

func TestCheckAndApplyBlocksDirtyWorktree(t *testing.T) {
	_, work := seedRepos(t)
	writeFile(t, work, "dirty.txt", "dirty\n")

	r := New(Config{Enabled: true, RepoDir: work, Remote: "origin", Branch: "main"}, nil)
	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.Reason != "worktree is dirty" {
		t.Fatalf("reason = %q, want dirty worktree", res.Reason)
	}
}

func TestCheckAndApplyNotifyDoesNotMerge(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled: true,
		RepoDir: work,
		Remote:  "origin",
		Branch:  "main",
		Mode:    ModeNotify,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "available" {
		t.Fatalf("status = %q, want available", res.Status)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after notify mode: %v", err)
	}
}

func TestCheckAndApplyDefaultModeDoesNotMerge(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled: true,
		RepoDir: work,
		Remote:  "origin",
		Branch:  "main",
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "available" {
		t.Fatalf("status = %q, want available (reason=%q)", res.Status, res.Reason)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after default mode: %v", err)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Sybra Test",
		"GIT_AUTHOR_EMAIL=sybra-test@example.invalid",
		"GIT_COMMITTER_NAME=Sybra Test",
		"GIT_COMMITTER_EMAIL=sybra-test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func seedRepos(t *testing.T) (upstream, work string) {
	t.Helper()
	root := t.TempDir()
	upstream = filepath.Join(root, "upstream")
	work = filepath.Join(root, "work")
	if err := os.Mkdir(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, upstream, "init", "-b", "main")
	writeFile(t, upstream, "README.md", "hello\n")
	gitTest(t, upstream, "add", "README.md")
	gitTest(t, upstream, "commit", "-m", "initial")
	gitTest(t, root, "clone", upstream, work)
	return upstream, work
}
