package autoupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
)

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
