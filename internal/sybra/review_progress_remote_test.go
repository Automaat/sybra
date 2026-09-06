package sybra

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentworkspace"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/reviewprogress"
)

func TestReviewProgressRecoveredTerminalCannotBindChangedInputs(t *testing.T) {
	for _, key := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64), ""} {
		sink := &recordingExecutionSink{ready: make(chan struct{}, 1)}
		run := &remoteExecution{runID: "accepted-run", sink: sink, reviewProgressKey: strings.Repeat("a", 64)}
		payload, err := json.Marshal(map[string]any{"state": "failed", "error": "interrupted", "artifactState": "none", "reviewProgressVerified": true, "reviewProgressKey": key})
		if err != nil {
			t.Fatal(err)
		}
		backend := &leaderExecutionBackend{}
		if !backend.completeAfterHandback(t.Context(), agent.ExecutionHandle("remote:accepted-run"), run, executioncontract.EventEnvelope{Payload: payload}) {
			t.Fatal("accepted paid run was not observed to completion")
		}
		if len(sink.events) != 1 || sink.events[0].Err == nil {
			t.Fatal("original terminal result lost")
		}
		if sink.events[0].ReviewProgressVerified != (key == run.reviewProgressKey) {
			t.Fatal("old or missing input key authorized progress under a new lease")
		}
	}
}

func TestReviewProgressRemoteBundlePinsDivergedComparison(t *testing.T) {
	leader, anchor := remoteBackendRepository(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	worker := filepath.Join(t.TempDir(), "stale-source")
	git(leader, "clone", "--no-hardlinks", leader, worker)
	git(leader, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "reviewed head")
	head := git(leader, "rev-parse", "HEAD")
	git(leader, "checkout", "--detach", anchor)
	git(leader, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "new comparison base")
	base := git(leader, "rev-parse", "HEAD")
	git(leader, "checkout", "--detach", head)
	comparison := &executioncontract.ReviewBase{Ref: "refs/remotes/origin/main", SHA: base, ProgressKey: strings.Repeat("a", 64)}
	content, ref, err := prepareRemoteWorkspaceBaseLimited(t.Context(), leader, "run-review", head, anchor, "refs/heads/main", executioncontract.MaxWorkspaceBaseBundleSize, comparison)
	if err != nil || ref == nil {
		t.Fatalf("missing bound bundle: %v", err)
	}
	spec := specForWorkspace(executioncontract.Workspace{
		RepositoryID: "repo", BaseSHA: head, BaseRef: "refs/heads/main", RepositoryAnchor: anchor,
		BaseBundle: ref, ReviewBase: comparison, Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree},
	})
	spec.RunID = "run-review"
	spec.Role = "review"
	layout, err := agentworkspace.PrepareWithBaseBundle(t.Context(), t.TempDir(), worker, spec, content)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewprogress.ValidateWorkspace(t.Context(), layout.Worktree, head, comparison.Ref, base); err != nil {
		t.Fatal(err)
	}
	if git(worker, "rev-parse", "HEAD") != anchor {
		t.Fatal("worker source mutated")
	}
	git(layout.Worktree, "update-ref", comparison.Ref, anchor)
	if err := reviewprogress.ValidateWorkspace(t.Context(), layout.Worktree, head, comparison.Ref, base); err == nil {
		t.Fatal("worker comparison drift accepted")
	}
}
