package completion

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// recordingWorkflow captures the workflow.CompletionWorkflow calls OnComplete
// makes, so a test can assert on advancement vs. suppression.
type recordingWorkflow struct {
	completed   []workflow.AgentCompletion
	cleared     []string
	rateLimited []string
}

func (r *recordingWorkflow) HandleAgentComplete(_ string, c workflow.AgentCompletion) {
	r.completed = append(r.completed, c)
}

func (r *recordingWorkflow) ClearAgentStep(_, agentID string) {
	r.cleared = append(r.cleared, agentID)
}

func (r *recordingWorkflow) RescheduleInterruptedAgent(_, _ string) {}

func (r *recordingWorkflow) RescheduleRateLimitedAgent(_, agentID string) {
	r.rateLimited = append(r.rateLimited, agentID)
}

func (r *recordingWorkflow) RescheduleCheckpointedAgent(_, _ string) {}

func (r *recordingWorkflow) ReschedulePromptUndeliveredAgent(_, _ string) {}

func (r *recordingWorkflow) DispatchEvent(_, _ string, _, _ map[string]string) (string, error) {
	return "", nil
}

// TestOnComplete_ParkedAdoptionSuppressesWorkflowAdvance covers the reattach
// adoption hazard: a survivor kept alive over a parked/terminal task status
// (agent.ParksLiveAgent) must still have its run result persisted, but must
// not advance the workflow — the engine gates advancement on Workflow.State
// alone, so an implementation workflow still waiting would otherwise run its
// next step and pull a task a human or the monitor parked (or that is already
// done) back into the pipeline.
func TestOnComplete_ParkedAdoptionSuppressesWorkflowAdvance(t *testing.T) {
	cases := []struct {
		name string
		// adoptedOver is the status reattach adopted the survivor over ("" for
		// an ordinary, non-adopted run).
		adoptedOver string
		// liveStatus is the task's status when the run completes.
		liveStatus  task.Status
		wantAdvance bool
	}{
		{
			name:        "adopted over done, still done",
			adoptedOver: "done",
			liveStatus:  task.StatusDone,
		},
		{
			name:        "adopted over human-required, still parked",
			adoptedOver: "human-required",
			liveStatus:  task.StatusHumanRequired,
		},
		{
			// A human put the task back in flight between reattach and
			// completion — this is an ordinary completion again.
			name:        "adopted over todo, since un-parked",
			adoptedOver: "todo",
			liveStatus:  task.StatusInProgress,
			wantAdvance: true,
		},
		{
			// Control: suppression is scoped to reattach adoptions. A normal
			// run whose workflow step legitimately drove the task to a
			// terminal status still advances.
			name:        "never adopted, terminal status",
			liveStatus:  task.StatusDone,
			wantAdvance: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := task.NewStore(dir)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			tasks := task.NewManager(store, nil)
			logger := discardLogger()
			wm := worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: tasks, Logger: logger})
			wf := &recordingWorkflow{}

			init := task.Update{}
			if tc.liveStatus == task.StatusHumanRequired {
				init.Escalation = task.OperatorDecisionEvidence("test.parked_adoption", "parked before reattachment")
				init.AutonomyOutcome = task.HumanRequiredOutcome()
			}
			created, err := tasks.CreateWithStatus("survivor task", "body", "headless", tc.liveStatus, init)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if err := tasks.AddRun(created.ID, task.AgentRun{AgentID: "ag-1", Role: "implementation", Mode: "headless"}); err != nil {
				t.Fatal(err)
			}

			ag := &agent.Agent{ID: "ag-1", TaskID: created.ID, Mode: "headless", Provider: "claude"}
			ag.SetAdoptedParkedStatus(tc.adoptedOver)
			ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "implementation finished"})

			h := New(Config{Logger: logger, Tasks: tasks, Worktrees: wm, WorkflowEngine: wf})
			h.OnComplete(ag)

			if got := len(wf.completed); got != boolToInt(tc.wantAdvance) {
				t.Fatalf("HandleAgentComplete called %d times, want %d", got, boolToInt(tc.wantAdvance))
			}
			if !tc.wantAdvance && len(wf.cleared) != 1 {
				t.Fatalf("ClearAgentStep called %d times, want 1 (a suppressed completion must release its step)", len(wf.cleared))
			}

			// The run's own result is retained either way — suppression only
			// blocks workflow advancement, it never discards the work.
			after, err := tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if len(after.AgentRuns) != 1 {
				t.Fatalf("agent runs = %d, want 1", len(after.AgentRuns))
			}
			if after.AgentRuns[0].Result != "implementation finished" {
				t.Fatalf("run result = %q, want the completed run's result persisted", after.AgentRuns[0].Result)
			}
		})
	}
}

// TestOnComplete_ParkedAdoptionSkipsFixReviewCompletion locks the second half
// of the adoption contract: a parked adoption may persist its run and clear
// its workflow step, and nothing else. handleFixReviewCompletion is the
// loudest side effect on that path — it pushes the survivor's branch (review
// hold off) and rewrites the task's status (review hold on), both behind the
// back of the human or monitor that parked the task. Review-hold mode is the
// observable half: the push target is a local bare clone whose refs a
// worktree commit already moves, so status is the signal that discriminates.
func TestOnComplete_ParkedAdoptionSkipsFixReviewCompletion(t *testing.T) {
	cases := []struct {
		name        string
		adoptedOver string
		liveStatus  task.Status
		wantStatus  task.Status
	}{
		{
			name:        "parked adoption is left alone",
			adoptedOver: "todo",
			liveStatus:  task.StatusTodo,
			wantStatus:  task.StatusTodo,
		},
		{
			// Control: an ordinary fix-review completion still runs the
			// handler, so review hold parks it for the human as designed.
			name:       "ordinary completion still holds for human",
			liveStatus: task.StatusInReview,
			wantStatus: task.StatusHumanRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := task.NewStore(dir)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			tasks := task.NewManager(store, nil)
			logger := discardLogger()
			wm := worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: tasks, Logger: logger})

			h := New(Config{Logger: logger, Tasks: tasks, Worktrees: wm})
			h.cfg = &config.Config{ReviewHold: config.ReviewHoldConfig{
				Enabled: true, Mode: config.ReviewHoldModeHold,
			}}

			tk, err := tasks.CreateWithStatus("fix pr", "body", "headless", tc.liveStatus, task.Update{})
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if err := tasks.AddRun(tk.ID, task.AgentRun{
				AgentID: "agent-1", Role: string(agent.RoleFixReview), Mode: "headless",
				State: string(agent.StateRunning), StartedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}

			ag := &agent.Agent{
				ID:        "agent-1",
				TaskID:    tk.ID,
				Mode:      "headless",
				Name:      agent.RoleFixReview.AgentName(tk.Title),
				StartedAt: time.Now(),
			}
			ag.SetAdoptedParkedStatus(tc.adoptedOver)
			ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "replies drafted"})
			h.OnComplete(ag)

			after, err := tasks.Get(tk.ID)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if after.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", after.Status, tc.wantStatus)
			}
		})
	}
}

// TestOnComplete_ParkedAdoptionSkipsReviewSideEffects locks the third leg of
// the adoption contract: markCompletedReview/salvageInterruptedReview must
// not run for a parked adoption. Both set task fields (Reviewed, CodeReview)
// outside the workflow engine's advancement path, which is the only place
// that records review evidence — a resumed simple-task-review would then see
// Reviewed=true and skip re-review, while require_evidence still finds the
// missing review criterion and parks the task again.
func TestOnComplete_ParkedAdoptionSkipsReviewSideEffects(t *testing.T) {
	cases := []struct {
		name         string
		adoptedOver  string
		liveStatus   task.Status
		wantReviewed bool
	}{
		{
			name:        "parked adoption does not mark reviewed",
			adoptedOver: "human-required",
			liveStatus:  task.StatusHumanRequired,
		},
		{
			// Control: an ordinary review completion still marks the task
			// reviewed, exactly as before this change.
			name:         "ordinary completion still marks reviewed",
			liveStatus:   task.StatusInProgress,
			wantReviewed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := task.NewStore(dir)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			tasks := task.NewManager(store, nil)
			logger := discardLogger()
			wm := worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: tasks, Logger: logger})
			wf := &recordingWorkflow{}

			init := task.Update{}
			if tc.liveStatus == task.StatusHumanRequired {
				init.Escalation = task.OperatorDecisionEvidence("test.parked_adoption", "parked before reattachment")
				init.AutonomyOutcome = task.HumanRequiredOutcome()
			}
			created, err := tasks.CreateWithStatus("review task", "body", "headless", tc.liveStatus, init)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if err := tasks.AddRun(created.ID, task.AgentRun{
				AgentID: "ag-1", Role: string(agent.RoleReview), Mode: "headless",
			}); err != nil {
				t.Fatal(err)
			}

			ag := &agent.Agent{ID: "ag-1", TaskID: created.ID, Mode: "headless", Provider: "claude", Name: agent.RoleReview.AgentName("review task")}
			ag.SetAdoptedParkedStatus(tc.adoptedOver)
			ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "review finished"})

			h := New(Config{Logger: logger, Tasks: tasks, Worktrees: wm, WorkflowEngine: wf})
			h.OnComplete(ag)

			after, err := tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			if after.Reviewed != tc.wantReviewed {
				t.Fatalf("Reviewed = %v, want %v", after.Reviewed, tc.wantReviewed)
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
