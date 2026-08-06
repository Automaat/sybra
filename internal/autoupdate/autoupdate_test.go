package autoupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
)

func TestGitUsesWorkingDirectoryAndDisablesPrompts(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$PWD\" \"$GIT_TERMINAL_PROMPT\"\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir)

	repoDir := t.TempDir()
	out, err := git(t.Context(), repoDir, "status", "--short")
	if err != nil {
		t.Fatalf("git: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[1] != "0" {
		t.Fatalf("git environment = %q, want working directory and prompt=0", out)
	}
	actualInfo, err := os.Stat(lines[0])
	if err != nil {
		t.Fatalf("stat actual working directory: %v", err)
	}
	wantInfo, err := os.Stat(repoDir)
	if err != nil {
		t.Fatalf("stat expected working directory: %v", err)
	}
	if !os.SameFile(actualInfo, wantInfo) {
		t.Fatalf("working directory = %q, want %q", lines[0], repoDir)
	}
}

func TestCheckAndApplyAutoModeFastForwards(t *testing.T) {
	ctx := t.Context()
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		PollInterval:   time.Hour,
		GateCommit:     greenGate,
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
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit:     greenGate,
		RequestRestart: func() {
			restarted = true
		},
	}, nil)

	r.check(t.Context())
	if !restarted {
		t.Fatal("restart was not requested after auto apply")
	}
}

func TestRunRequestsRestartWhenPostApplyStateSaveFails(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	stateFile := filepath.Join(work, ".git", "autoupdate-state.json")
	hook := filepath.Join(work, ".git", "hooks", "post-merge")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nchmod 000 .git/autoupdate-state.json\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stateFile, 0o644)
	})

	restarted := false
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		StateFile:      stateFile,
		GateCommit:     greenGate,
		RequestRestart: func() {
			restarted = true
		},
	}, nil)

	r.check(t.Context())
	if !restarted {
		t.Fatal("restart was not requested after post-apply state save failure")
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); err != nil {
		t.Fatalf("feature.txt missing after auto mode: %v", err)
	}
}

func TestRunTriggerCheckAppliesImmediately(t *testing.T) {
	upstream, work := seedRepos(t)

	restarted := make(chan struct{}, 1)
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		PollInterval:   time.Hour,
		GateCommit:     greenGate,
		RequestRestart: func() {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r.TriggerCheck()

	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("triggered check did not request restart")
	}

	if _, err := os.Stat(filepath.Join(work, "feature.txt")); err != nil {
		t.Fatalf("feature.txt missing after triggered auto update: %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
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

func TestSaveStateAtomicReplace(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "autoupdate-state.json")
	initial := persistedState{CandidateSHA: "old", CandidateState: "pending"}
	if err := saveState(path, initial); err != nil {
		t.Fatalf("saveState(initial): %v", err)
	}

	next := persistedState{CandidateSHA: "new", CandidateState: "approved"}
	if err := saveState(path, next); err != nil {
		t.Fatalf("saveState(next): %v", err)
	}

	got, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState(): %v", err)
	}
	if got.CandidateSHA != next.CandidateSHA || got.CandidateState != next.CandidateState {
		t.Fatalf("state = %+v, want %+v", got, next)
	}

	tmpFiles, err := filepath.Glob(path + ".tmp-*")
	if err != nil {
		t.Fatalf("Glob(): %v", err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("temp files left behind: %v", tmpFiles)
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

func TestCheckAndApplyBlocksUnwritableGitObjectDatabase(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through permission bits")
	}
	_, work := seedRepos(t)
	objectsRel := gitTestOutput(t, work, "rev-parse", "--git-path", "objects")
	objectsDir := filepath.Join(work, objectsRel)
	if err := os.Chmod(objectsDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(objectsDir, 0o755)
	})

	r := New(Config{Enabled: true, RepoDir: work, Remote: "origin", Branch: "main"}, nil)
	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if !strings.Contains(res.Reason, "git object database is not writable") {
		t.Fatalf("reason = %q, want object database repair guidance", res.Reason)
	}
}

func TestCheckAndApplyBlocksUnwritableGitObjectFanoutDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through permission bits")
	}
	_, work := seedRepos(t)
	objectsRel := gitTestOutput(t, work, "rev-parse", "--git-path", "objects")
	objectsDir := filepath.Join(work, objectsRel)
	var fanoutDir string
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) == 2 {
			fanoutDir = filepath.Join(objectsDir, entry.Name())
			break
		}
	}
	if fanoutDir == "" {
		t.Fatal("seed repo has no loose object fanout directory")
	}
	if err := os.Chmod(fanoutDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(fanoutDir, 0o755)
	})

	r := New(Config{Enabled: true, RepoDir: work, Remote: "origin", Branch: "main"}, nil)
	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if !strings.Contains(res.Reason, "git object fanout directory is not writable") {
		t.Fatalf("reason = %q, want fanout directory repair guidance", res.Reason)
	}
}

func TestCheckAndApplyBlocksUnwritableGitObjectPackDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through permission bits")
	}
	_, work := seedRepos(t)
	objectsRel := gitTestOutput(t, work, "rev-parse", "--git-path", "objects")
	packDir := filepath.Join(work, objectsRel, "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(packDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(packDir, 0o755)
	})

	r := New(Config{Enabled: true, RepoDir: work, Remote: "origin", Branch: "main"}, nil)
	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if !strings.Contains(res.Reason, "git object fanout directory is not writable") ||
		!strings.Contains(res.Reason, string(filepath.Separator)+"pack") {
		t.Fatalf("reason = %q, want pack directory repair guidance", res.Reason)
	}
}

func TestCheckAndApplyNotifyDoesNotMerge(t *testing.T) {
	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeNotify,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit:     greenGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "approved" {
		t.Fatalf("status = %q, want approved", res.Status)
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
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit:     greenGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "approved" {
		t.Fatalf("status = %q, want approved (reason=%q)", res.Status, res.Reason)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after default mode: %v", err)
	}
}

func TestCheckAndApplyRejectsAutoModeWithoutRequiredChecks(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:    true,
		RepoDir:    work,
		Remote:     "origin",
		Branch:     "main",
		Mode:       ModeAuto,
		Repository: "o/r",
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "rejected" || res.Reason != "required checks are empty" {
		t.Fatalf("result = %+v, want rejected/required checks are empty", res)
	}
}

func TestCheckAndApplyWaitsForPendingChecks(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeNotify,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit:     pendingGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "waiting" || res.Reason != "pending required checks: test" {
		t.Fatalf("result = %+v, want waiting/pending required checks: test", res)
	}
}

func TestEnsureGithubTokenAllowsAmbientAuthWhenAppDisabled(t *testing.T) {
	github.DisableAppAuth()
	t.Cleanup(github.DisableAppAuth)

	r := New(Config{Mode: ModeAuto}, nil)
	state := persistedState{}

	if got := r.ensureGithubToken(t.Context(), Config{}, &state, "o/r", "abc", "def", []string{"README.md"}); got != nil {
		t.Fatalf("ensureGithubToken() = %+v, want nil", got)
	}
	if state.CandidateState != "" {
		t.Fatalf("CandidateState = %q, want empty", state.CandidateState)
	}
}

func TestCheckAndApplyRejectsFailedChecks(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeNotify,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit:     failedGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "rejected" || res.Reason != "failed required checks: test" {
		t.Fatalf("result = %+v, want rejected/failed required checks: test", res)
	}
}

func TestCheckAndApplyOverrideBypassesGateOnce(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	override := filepath.Join(t.TempDir(), "autoupdate-override")
	if err := os.WriteFile(override, []byte("override\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		OverrideFile:   override,
		GateCommit:     failedGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "applied" {
		t.Fatalf("status = %q, want applied (reason=%q)", res.Status, res.Reason)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("override file still exists: %v", err)
	}
}

func TestCheckAndApplyNotifyOverrideIsConsumed(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	override := filepath.Join(t.TempDir(), "autoupdate-override")
	if err := os.WriteFile(override, []byte("override\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeNotify,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		OverrideFile:   override,
		GateCommit:     failedGate,
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "approved" || res.Reason != "manual override" {
		t.Fatalf("result = %+v, want approved/manual override", res)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after notify mode: %v", err)
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("override file still exists: %v", err)
	}
}

func TestCheckAndApplyDoesNotMergeWhenOverrideCannotBeConsumed(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	overrideDir := t.TempDir()
	override := filepath.Join(overrideDir, "autoupdate-override")
	if err := os.WriteFile(override, []byte("override\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(overrideDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(overrideDir, 0o755)
	})

	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		OverrideFile:   override,
		GateCommit:     failedGate,
	}, nil)

	if _, err := r.CheckAndApply(t.Context()); err == nil {
		t.Fatal("CheckAndApply() err = nil, want override clear failure")
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after failed override consumption: %v", err)
	}
	if _, err := os.Stat(override); err != nil {
		t.Fatalf("override file missing after failed consumption: %v", err)
	}
}

func TestCheckAndApplyPersistsApprovedStateBeforeAutoApply(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "new\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	stateFile := filepath.Join(t.TempDir(), "autoupdate-state.json")
	remoteMoved := false
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		StateFile:      stateFile,
		GateCommit: func(ctx context.Context, repo, sha string, required []string) (github.CommitGate, error) {
			remoteMoved = true
			if err := os.Rename(upstream, upstream+"-moved"); err != nil {
				t.Fatalf("rename upstream: %v", err)
			}
			return greenGate(ctx, repo, sha, required)
		},
	}, nil)

	if _, err := r.CheckAndApply(t.Context()); err == nil {
		t.Fatal("CheckAndApply() err = nil, want re-resolve failure")
	}
	if !remoteMoved {
		t.Fatal("gate hook did not run")
	}
	state, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if state.CandidateState != "approved" {
		t.Fatalf("CandidateState = %q, want approved", state.CandidateState)
	}
	if state.PendingSHA == "" {
		t.Fatal("PendingSHA = empty, want candidate sha")
	}
}

func TestCheckAndApplyWaitsWhenCandidateIsSuperseded(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	writeFile(t, upstream, "feature.txt", "one\n")
	gitTest(t, upstream, "add", "feature.txt")
	gitTest(t, upstream, "commit", "-m", "add feature")

	var transitions []string
	r := New(Config{
		Enabled:        true,
		RepoDir:        work,
		Remote:         "origin",
		Branch:         "main",
		Mode:           ModeAuto,
		Repository:     "o/r",
		RequiredChecks: []string{"test"},
		GateCommit: func(ctx context.Context, repo, sha string, required []string) (github.CommitGate, error) {
			writeFile(t, upstream, "feature-2.txt", "two\n")
			gitTest(t, upstream, "add", "feature-2.txt")
			gitTest(t, upstream, "commit", "-m", "add second feature")
			return greenGate(ctx, repo, sha, required)
		},
		AuditTransition: func(data map[string]any) {
			transitions = append(transitions, data["transition"].(string))
		},
	}, nil)

	res, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "waiting" || res.Reason != "candidate changed before apply" {
		t.Fatalf("result = %+v, want waiting/candidate changed before apply", res)
	}
	if _, err := os.Stat(filepath.Join(work, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature.txt exists after superseded candidate: %v", err)
	}
	if !slices.Equal(transitions, []string{"seen", "approved", "superseded"}) {
		t.Fatalf("transitions = %v, want [seen approved superseded]", transitions)
	}
}

func TestCheckAndApplyAuditsTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     string
		gate     func(context.Context, string, string, []string) (github.CommitGate, error)
		want     []string
		wantLast string
	}{
		{
			name:     "approved and applied",
			mode:     ModeAuto,
			gate:     greenGate,
			want:     []string{"seen", "approved", "applied"},
			wantLast: "applied",
		},
		{
			name:     "pending",
			mode:     ModeNotify,
			gate:     pendingGate,
			want:     []string{"seen", "waiting"},
			wantLast: "waiting",
		},
		{
			name:     "rejected",
			mode:     ModeNotify,
			gate:     failedGate,
			want:     []string{"seen", "rejected"},
			wantLast: "rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			upstream, work := seedRepos(t)
			writeFile(t, upstream, "feature.txt", "new\n")
			gitTest(t, upstream, "add", "feature.txt")
			gitTest(t, upstream, "commit", "-m", "add feature")

			var transitions []string
			r := New(Config{
				Enabled:        true,
				RepoDir:        work,
				Remote:         "origin",
				Branch:         "main",
				Mode:           tt.mode,
				Repository:     "o/r",
				RequiredChecks: []string{"test"},
				GateCommit:     tt.gate,
				AuditTransition: func(data map[string]any) {
					transitions = append(transitions, data["transition"].(string))
				},
			}, nil)

			res, err := r.CheckAndApply(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(transitions, tt.want) {
				t.Fatalf("transitions = %v, want %v (result=%+v)", transitions, tt.want, res)
			}
		})
	}
}

func TestCheckAndApplyCoalescesRestartsWithinInterval(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	stateFile := filepath.Join(t.TempDir(), "autoupdate-state.json")
	commit := func(name, body string) {
		writeFile(t, upstream, name, body)
		gitTest(t, upstream, "add", name)
		gitTest(t, upstream, "commit", "-m", "add "+name)
	}

	clockNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	restarts := 0
	r := New(Config{
		Enabled:          true,
		RepoDir:          work,
		Remote:           "origin",
		Branch:           "main",
		Mode:             ModeAuto,
		Repository:       "o/r",
		RequiredChecks:   []string{"test"},
		StateFile:        stateFile,
		CoalesceInterval: time.Hour,
		GateCommit:       greenGate,
		Now:              func() time.Time { return clockNow },
		RequestRestart:   func() { restarts++ },
	}, nil)

	commit("feature-1.txt", "one\n")
	r.check(t.Context())
	if restarts != 1 {
		t.Fatalf("restarts after first apply = %d, want 1", restarts)
	}
	firstRestartAt := clockNow

	// Two more merges land within the coalesce window: both are held, not applied.
	clockNow = clockNow.Add(10 * time.Minute)
	commit("feature-2.txt", "two\n")
	res2, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "coalesced" || res2.CoalescedCount != 1 {
		t.Fatalf("res2 = %+v, want coalesced/CoalescedCount=1", res2)
	}
	if res2.PendingSHA == "" {
		t.Fatal("res2.PendingSHA empty, want held candidate sha")
	}
	if want := firstRestartAt.Add(time.Hour); !res2.NextRestartEligibleAt.Equal(want) {
		t.Fatalf("res2.NextRestartEligibleAt = %v, want %v", res2.NextRestartEligibleAt, want)
	}
	if _, err := os.Stat(filepath.Join(work, "feature-2.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-2.txt merged despite coalescing: %v", err)
	}

	clockNow = clockNow.Add(10 * time.Minute)
	commit("feature-3.txt", "three\n")
	res3, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res3.Status != "coalesced" || res3.CoalescedCount != 2 {
		t.Fatalf("res3 = %+v, want coalesced/CoalescedCount=2", res3)
	}
	r.check(t.Context())
	if restarts != 1 {
		t.Fatalf("restarts while still inside coalesce window = %d, want still 1", restarts)
	}

	// Once the interval elapses, exactly one restart fires and the newest
	// candidate (not the first-seen one) is the one that gets applied.
	clockNow = clockNow.Add(45 * time.Minute)
	r.check(t.Context())
	if restarts != 2 {
		t.Fatalf("restarts after interval elapsed = %d, want 2", restarts)
	}
	if _, err := os.Stat(filepath.Join(work, "feature-3.txt")); err != nil {
		t.Fatalf("feature-3.txt missing after coalesced apply: %v", err)
	}
	state, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if state.CoalescedCount != 0 {
		t.Fatalf("CoalescedCount after apply = %d, want reset to 0", state.CoalescedCount)
	}
}

func TestCheckAndApplyCoalesceThrottlePersistsAcrossRunnerRecreation(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	stateFile := filepath.Join(t.TempDir(), "autoupdate-state.json")

	clockNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newRunner := func() *Runner {
		return New(Config{
			Enabled:          true,
			RepoDir:          work,
			Remote:           "origin",
			Branch:           "main",
			Mode:             ModeAuto,
			Repository:       "o/r",
			RequiredChecks:   []string{"test"},
			StateFile:        stateFile,
			CoalesceInterval: time.Hour,
			GateCommit:       greenGate,
			Now:              func() time.Time { return clockNow },
		}, nil)
	}

	writeFile(t, upstream, "feature-1.txt", "one\n")
	gitTest(t, upstream, "add", "feature-1.txt")
	gitTest(t, upstream, "commit", "-m", "add feature-1")

	res1, err := newRunner().CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res1.Status != "applied" {
		t.Fatalf("res1 = %+v, want applied", res1)
	}

	// A brand new Runner (simulating a process restart) must still see the
	// persisted LastRestartAt and hold the next candidate — the throttle
	// lives in the state file, not in-memory Runner state.
	clockNow = clockNow.Add(10 * time.Minute)
	writeFile(t, upstream, "feature-2.txt", "two\n")
	gitTest(t, upstream, "add", "feature-2.txt")
	gitTest(t, upstream, "commit", "-m", "add feature-2")

	res2, err := newRunner().CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "coalesced" {
		t.Fatalf("res2 = %+v, want coalesced (throttle must survive runner recreation)", res2)
	}
}

func TestCheckAndApplyOverrideBypassesCoalesceGate(t *testing.T) {
	t.Parallel()

	upstream, work := seedRepos(t)
	stateFile := filepath.Join(t.TempDir(), "autoupdate-state.json")
	overrideFile := filepath.Join(t.TempDir(), "autoupdate-override")

	clockNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := New(Config{
		Enabled:          true,
		RepoDir:          work,
		Remote:           "origin",
		Branch:           "main",
		Mode:             ModeAuto,
		Repository:       "o/r",
		RequiredChecks:   []string{"test"},
		StateFile:        stateFile,
		OverrideFile:     overrideFile,
		CoalesceInterval: time.Hour,
		GateCommit:       greenGate,
		Now:              func() time.Time { return clockNow },
	}, nil)

	writeFile(t, upstream, "feature-1.txt", "one\n")
	gitTest(t, upstream, "add", "feature-1.txt")
	gitTest(t, upstream, "commit", "-m", "add feature-1")

	res1, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res1.Status != "applied" {
		t.Fatalf("res1 = %+v, want applied", res1)
	}

	// A second update lands minutes later, well inside the coalesce window —
	// an operator writes the override marker to force it through immediately.
	clockNow = clockNow.Add(5 * time.Minute)
	writeFile(t, upstream, "feature-2.txt", "two\n")
	gitTest(t, upstream, "add", "feature-2.txt")
	gitTest(t, upstream, "commit", "-m", "add feature-2")
	if err := os.WriteFile(overrideFile, []byte("override\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res2, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "applied" || res2.RestartReason != "manual override" {
		t.Fatalf("res2 = %+v, want applied/manual override", res2)
	}
	if _, err := os.Stat(overrideFile); !os.IsNotExist(err) {
		t.Fatalf("override file still exists after bypass: %v", err)
	}
	state, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if state.CoalescedCount != 0 {
		t.Fatalf("CoalescedCount after override apply = %d, want reset to 0", state.CoalescedCount)
	}

	// A third update within the window, with no override this time, must be
	// coalesced again — the one-shot override must not disable coalescing
	// for subsequent checks.
	clockNow = clockNow.Add(5 * time.Minute)
	writeFile(t, upstream, "feature-3.txt", "three\n")
	gitTest(t, upstream, "add", "feature-3.txt")
	gitTest(t, upstream, "commit", "-m", "add feature-3")

	res3, err := r.CheckAndApply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res3.Status != "coalesced" || res3.CoalescedCount != 1 {
		t.Fatalf("res3 = %+v, want coalesced/CoalescedCount=1", res3)
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
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
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

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Sybra Test",
		"GIT_AUTHOR_EMAIL=sybra-test@example.invalid",
		"GIT_COMMITTER_NAME=Sybra Test",
		"GIT_COMMITTER_EMAIL=sybra-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
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

func greenGate(_ context.Context, repo, sha string, required []string) (github.CommitGate, error) {
	return gateWithState(repo, sha, required, "SUCCESS")
}

func pendingGate(_ context.Context, repo, sha string, required []string) (github.CommitGate, error) {
	return gateWithState(repo, sha, required, "PENDING")
}

func failedGate(_ context.Context, repo, sha string, required []string) (github.CommitGate, error) {
	return gateWithState(repo, sha, required, "FAILURE")
}

func gateWithState(repo, sha string, required []string, state string) (github.CommitGate, error) {
	checks := make(map[string]string, len(required))
	gate := github.CommitGate{
		Repo:   repo,
		SHA:    sha,
		Checks: checks,
	}
	for _, check := range required {
		checks[check] = state
		switch state {
		case "SUCCESS":
			gate.Succeeded = append(gate.Succeeded, check)
		case "PENDING":
			gate.Pending = append(gate.Pending, check)
		case "FAILURE":
			gate.Failed = append(gate.Failed, check)
		}
	}
	return gate, nil
}
