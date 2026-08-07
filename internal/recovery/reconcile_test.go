package recovery_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

type fakePRResolver struct {
	ref   recovery.PRRef
	err   error
	calls int
}

func (f *fakePRResolver) ResolvePRForTask(context.Context, string, string, string) (recovery.PRRef, error) {
	f.calls++
	return f.ref, f.err
}

func newLostPROrphan(t *testing.T, tasks *task.Manager, mutate func(*task.Update)) string {
	t.Helper()
	created, err := tasks.Create("lost pr number", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	status := task.StatusInReview
	branch := "feat/thing-abcd1234"
	proj := "owner/repo"
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: task.Ptr(time.Now().UTC()),
	}
	u := task.Update{Status: &status, Branch: &branch, ProjectID: &proj, Workflow: &wf}
	if mutate != nil {
		mutate(&u)
	}
	if _, err := tasks.Update(created.ID, u); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	return created.ID
}

func newReconcileRecovery(t *testing.T, tasks *task.Manager, resolver recovery.PRResolver) (*recovery.Recovery, *sync.WaitGroup) {
	t.Helper()
	logger := discardLogger()
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})
	var wg sync.WaitGroup
	r := &recovery.Recovery{
		Tasks:     tasks,
		Agents:    agents,
		Worktrees: wm,
		PRs:       resolver,
		Logger:    logger,
		Throttle:  logging.NewErrorThrottle(),
		WG:        &wg,
	}
	return r, &wg
}

func TestReconcileLostPRNumber(t *testing.T) {
	cases := []struct {
		name            string
		mutate          func(*task.Update)
		ref             recovery.PRRef
		resolveErr      error
		wantStatus      task.Status
		wantPR          int
		wantWorkflowNil bool
	}{
		{
			name:            "merged PR advances to done and clears workflow",
			ref:             recovery.PRRef{Number: 1775, State: "MERGED"},
			wantStatus:      task.StatusDone,
			wantPR:          1775,
			wantWorkflowNil: true,
		},
		{
			name:       "open PR backfills pr_number and stays in-review",
			ref:        recovery.PRRef{Number: 42, State: "OPEN"},
			wantStatus: task.StatusInReview,
			wantPR:     42,
		},
		{
			name: "ready-review merged PR advances to done and clears workflow",
			mutate: func(u *task.Update) {
				u.Status = task.Ptr(task.StatusReadyReview)
			},
			ref:             recovery.PRRef{Number: 2469, State: "MERGED"},
			wantStatus:      task.StatusDone,
			wantPR:          2469,
			wantWorkflowNil: true,
		},
		{
			name: "ready-pr open PR backfills pr_number and preserves status",
			mutate: func(u *task.Update) {
				u.Status = task.Ptr(task.StatusReadyPR)
			},
			ref:        recovery.PRRef{Number: 2470, State: "OPEN"},
			wantStatus: task.StatusReadyPR,
			wantPR:     2470,
		},
		{
			name:       "no matching PR leaves the task unchanged",
			ref:        recovery.PRRef{},
			wantStatus: task.StatusInReview,
			wantPR:     0,
		},
		{
			name:       "resolver error leaves the task unchanged",
			resolveErr: errors.New("gh unavailable"),
			wantStatus: task.StatusInReview,
			wantPR:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := task.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			tasks := task.NewManager(store, nil)
			id := newLostPROrphan(t, tasks, tc.mutate)

			resolver := &fakePRResolver{ref: tc.ref, err: tc.resolveErr}
			r, wg := newReconcileRecovery(t, tasks, resolver)
			r.ReconcileLostPRNumber(context.Background())
			wg.Wait()

			if resolver.calls != 1 {
				t.Fatalf("resolver calls = %d, want 1", resolver.calls)
			}
			got, err := tasks.Get(id)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if got.PRNumber != tc.wantPR {
				t.Errorf("pr_number = %d, want %d", got.PRNumber, tc.wantPR)
			}
			if tc.wantWorkflowNil && got.Workflow != nil {
				t.Errorf("workflow = %+v, want nil (cleared)", got.Workflow)
			}
			if !tc.wantWorkflowNil && got.Workflow == nil {
				t.Errorf("workflow was cleared but should have been preserved")
			}
			if tc.wantStatus == task.StatusDone && got.Outcome != "merged" {
				t.Errorf("outcome = %q, want merged", got.Outcome)
			}
		})
	}
}

func TestReconcileLostPRNumberSkipsIneligible(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*task.Update)
	}{
		{
			name:   "pr_number already linked",
			mutate: func(u *task.Update) { u.PRNumber = task.Ptr(99) },
		},
		{
			name:   "review-tagged task",
			mutate: func(u *task.Update) { u.Tags = task.Ptr([]string{"review"}) },
		},
		{
			name:   "no branch to resolve from",
			mutate: func(u *task.Update) { u.Branch = task.Ptr("") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := task.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			tasks := task.NewManager(store, nil)
			newLostPROrphan(t, tasks, tc.mutate)

			resolver := &fakePRResolver{ref: recovery.PRRef{Number: 1, State: "MERGED"}}
			r, wg := newReconcileRecovery(t, tasks, resolver)
			r.ReconcileLostPRNumber(context.Background())
			wg.Wait()

			if resolver.calls != 0 {
				t.Fatalf("resolver called %d times for ineligible task, want 0", resolver.calls)
			}
		})
	}
}

func TestReconcileLostPRNumberNilResolver(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	tasks := task.NewManager(store, nil)
	id := newLostPROrphan(t, tasks, nil)

	r, wg := newReconcileRecovery(t, tasks, nil)
	r.ReconcileLostPRNumber(context.Background())
	wg.Wait()

	got, err := tasks.Get(id)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %s, want in-review (nil resolver must no-op)", got.Status)
	}
}
