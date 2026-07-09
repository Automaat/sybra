package agentorch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

func mustRunInDirDC(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// initConflictingRepo builds a bare repo (with a live "origin" remote back to
// its src checkout) exactly like worktree's own
// TestPrepareForTask_RebaseConflictFailsClosed harness: bare's origin.fetch
// refspec is pre-configured and pre-fetched so a later plain commit into src
// (no push needed) is picked up the next time PrepareForTask fetches origin
// inside bare, forcing a real rebase conflict.
func initConflictingRepo(t *testing.T) (bare, src string) {
	t.Helper()
	src = t.TempDir()
	mustRunInDirDC(t, "", "git", "init", "-b", "main", src)
	mustRunInDirDC(t, src, "git", "config", "user.email", "test@test.com")
	mustRunInDirDC(t, src, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDirDC(t, src, "git", "add", ".")
	mustRunInDirDC(t, src, "git", "commit", "-m", "init")

	bare = filepath.Join(t.TempDir(), "origin.git")
	mustRunInDirDC(t, "", "git", "clone", "--bare", src, bare)
	mustRunInDirDC(t, bare, "git", "-c", "safe.bareRepository=all", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	mustRunInDirDC(t, bare, "git", "-c", "safe.bareRepository=all", "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return bare, src
}

func writeTestProject(t *testing.T, projDir string, p project.Project) {
	t.Helper()
	data, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, projectFileName(p.ID))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func projectFileName(id string) string {
	out := make([]byte, 0, len(id))
	for i := range len(id) {
		if id[i] == '/' {
			out = append(out, '-', '-')
			continue
		}
		out = append(out, id[i])
	}
	return string(out) + ".yaml"
}

// TestStartAgentWithAssignment_ReleasesClaimBeforeConflictRecovery pins the
// fix for c1778673: a branch-conflict-fix recovery triggered synchronously
// from inside StartAgentWithAssignment's own worktree-prep failure path must
// see the per-task dispatch claim already released, or its "fix" step dispatch
// (which re-enters this same StartAgentWithAssignment choke point for the
// same taskID) always observes the claim as held and bails with
// workflow.ErrDispatchInFlight — the branch-conflict-fix workflow parks the
// task human-required after 3 retries without ever starting a fix agent.
func TestStartAgentWithAssignment_ReleasesClaimBeforeConflictRecovery(t *testing.T) {
	bare, src := initConflictingRepo(t)

	projDir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clonesDir := filepath.Join(t.TempDir(), "clones")
	projStore, err := project.NewStore(projDir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}
	proj := project.Project{
		ID:        "test/proj",
		Name:      "proj",
		Owner:     "test",
		Repo:      "proj",
		URL:       bare,
		ClonePath: bare,
		Type:      project.ProjectTypePet,
		Status:    project.ProjectStatusReady,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	writeTestProject(t, projDir, proj)

	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(taskStore, nil)

	wtMgr := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Projects:     projStore,
		Tasks:        tasks,
		Logger:       discardSlogLogger(),
		LogsDir:      t.TempDir(),
		SetupTimeout: 30 * time.Second,
	})

	tk, err := tasks.Create("conflicting task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// First prepare creates the worktree and its own branch commit.
	wtPath, err := wtMgr.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	mustRunInDirDC(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDirDC(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDirDC(t, wtPath, "git", "add", "README.md")
	mustRunInDirDC(t, wtPath, "git", "commit", "-m", "branch edit")

	// Diverge upstream (src) so the next PrepareForTask's fetch+rebase inside
	// bare fails closed with a real content conflict.
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDirDC(t, src, "git", "add", "README.md")
	mustRunInDirDC(t, src, "git", "commit", "-m", "upstream edit")

	agentMgr, err := agent.NewManager(context.Background(), nil, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	o := New(tasks, projStore, agentMgr, nil, discardSlogLogger(), wtMgr, &config.Config{})

	var (
		recoveryCalled       bool
		claimHeldOnRecovery  bool
		claimAvailableToTake bool
	)
	o.SetConflictRecovery(func(taskID string) bool {
		recoveryCalled = true
		// If the outer claim were still held (the bug), this would fail.
		claimAvailableToTake = agentMgr.ClaimTaskDispatch(taskID)
		claimHeldOnRecovery = !claimAvailableToTake
		if claimAvailableToTake {
			agentMgr.ReleaseTaskDispatch(taskID)
		}
		// Decline recovery (return false) so the caller falls through to its
		// normal error path instead of this test needing to fake an entire
		// fix-agent dispatch.
		return false
	})

	_, _, err = o.StartAgentWithAssignment(tk.ID, "headless", "prompt", false, false, "", workflow.AgentAssignment{})
	if err == nil {
		t.Fatal("StartAgentWithAssignment err = nil, want an error (worktree required for project task)")
	}
	if !recoveryCalled {
		t.Fatal("conflict recovery callback was never invoked — rebase failure did not reach recovery path")
	}
	if claimHeldOnRecovery {
		t.Fatal("dispatch claim was still held when conflict recovery ran — a nested fix-agent dispatch for this task would observe ErrDispatchInFlight and never start")
	}

	// The outer claim must also be free after the call returns (no leak from
	// the early release + deferred double-release guard).
	if !agentMgr.ClaimTaskDispatch(tk.ID) {
		t.Fatal("dispatch claim leaked after StartAgentWithAssignment returned")
	}
	agentMgr.ReleaseTaskDispatch(tk.ID)
}
