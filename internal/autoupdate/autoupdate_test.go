package autoupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckAndApplyFastForwardsAndWritesMarker(t *testing.T) {
	ctx := t.Context()
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	marker := filepath.Join(t.TempDir(), RestartMarker)
	r := New(Config{
		Enabled:           true,
		RepoDir:           work,
		Remote:            "origin",
		Branch:            "main",
		Mode:              ModeAuto,
		PollInterval:      time.Hour,
		RestartMarkerPath: marker,
	}, nil, nil)

	res, err := r.CheckAndApply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "updated" {
		t.Fatalf("status = %q, want updated (reason=%q)", res.Status, res.Reason)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("restart marker missing: %v", err)
	}
	if got := readFile(t, work, "feature.txt"); got != "new\n" {
		t.Fatalf("feature.txt = %q, want new", got)
	}
}

func TestCheckAndApplyBlocksDirtyWorktree(t *testing.T) {
	_, work := seedRepos(t)
	writeFile(t, work, "dirty.txt", "dirty\n")

	r := New(Config{Enabled: true, RepoDir: work, Remote: "origin", Branch: "main"}, nil, nil)
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

	marker := filepath.Join(t.TempDir(), RestartMarker)
	r := New(Config{
		Enabled:           true,
		RepoDir:           work,
		Remote:            "origin",
		Branch:            "main",
		Mode:              ModeNotify,
		RestartMarkerPath: marker,
	}, nil, nil)

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
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("restart marker exists in notify mode: %v", err)
	}
}

func TestCheckAndApplyNotifyIgnoresRestartBlock(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	calls := 0
	r := New(Config{
		Enabled: true,
		RepoDir: work,
		Remote:  "origin",
		Branch:  "main",
		Mode:    ModeNotify,
		BlockRestart: func() string {
			calls++
			return "active agents running"
		},
	}, nil, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "available" {
		t.Fatalf("status = %q, want available (reason=%q)", res.Status, res.Reason)
	}
	if calls != 0 {
		t.Fatalf("BlockRestart called %d times in notify mode; want 0", calls)
	}
}

func TestCheckAndApplyAutoBlocksBeforeMerge(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	marker := filepath.Join(t.TempDir(), RestartMarker)
	r := New(Config{
		Enabled:           true,
		RepoDir:           work,
		Remote:            "origin",
		Branch:            "main",
		Mode:              ModeAuto,
		RestartMarkerPath: marker,
		BlockRestart: func() string {
			return "active agents running"
		},
	}, nil, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if res.Reason != "active agents running" {
		t.Fatalf("reason = %q, want active agents running", res.Reason)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists despite restart block: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("restart marker exists despite restart block: %v", err)
	}
}

func TestCheckDefersRestartUntilBlockClears(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	calls := 0
	restarted := false
	r := New(Config{
		Enabled:           true,
		RepoDir:           work,
		Remote:            "origin",
		Branch:            "main",
		Mode:              ModeAuto,
		PollInterval:      time.Millisecond,
		RestartMarkerPath: filepath.Join(t.TempDir(), RestartMarker),
		BlockRestart: func() string {
			calls++
			if calls == 1 {
				return ""
			}
			if calls < 4 {
				return "active agents running"
			}
			return ""
		},
	}, nil, func() { restarted = true })

	r.check(t.Context())
	if !restarted {
		t.Fatal("restart callback was not called after block cleared")
	}
	if calls != 4 {
		t.Fatalf("BlockRestart calls = %d, want 4", calls)
	}
}

func TestCheckAndApplyDoesNotMergeWhenMarkerFails(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	blockingFile := filepath.Join(t.TempDir(), "not-a-dir")
	writeFile(t, filepath.Dir(blockingFile), filepath.Base(blockingFile), "x")
	marker := filepath.Join(blockingFile, RestartMarker)

	r := New(Config{
		Enabled:           true,
		RepoDir:           work,
		Remote:            "origin",
		Branch:            "main",
		RestartMarkerPath: marker,
	}, nil, nil)

	res, err := r.CheckAndApply(t.Context())
	if err == nil {
		t.Fatal("expected marker failure")
	}
	if res.Status != "" {
		t.Fatalf("status = %q, want empty result on marker failure", res.Status)
	}
	if _, statErr := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("feature.txt exists despite marker failure: %v", statErr)
	}
}

func TestWriteRestartMarkerRejectsEmptyAndRelativePath(t *testing.T) {
	for _, path := range []string{"", "relative/restart-requested"} {
		if err := WriteRestartMarker(path); err == nil {
			t.Fatalf("WriteRestartMarker(%q) expected error", path)
		}
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
