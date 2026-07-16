package sybra

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
)

func setupPromptLabCoordinator(t *testing.T, cfg *config.Config, approve func(string) error) *promptLabCoordinator {
	t.Helper()
	c := newPromptLabCoordinatorForTest(t, cfg, approve)
	if _, err := c.projects.CreateMeta("https://github.com/Automaat/sybra.git", project.ProjectTypePet); err != nil {
		t.Fatalf("CreateMeta: %v", err)
	}
	return c
}

func newPromptLabCoordinatorForTest(t *testing.T, cfg *config.Config, approve func(string) error) *promptLabCoordinator {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newPromptLabCoordinator(
		task.NewManager(store, nil),
		projects,
		nil,
		slog.New(slog.DiscardHandler),
		cfg,
		func(project.ProjectType) bool { return true },
		func(string) *WorkScrubContext { return nil },
		approve,
	)
}

func weakProposal(verdict promptlab.OfflineVerdict) promptlab.Proposal {
	return promptlab.Proposal{
		ID:      "pl-a2d853b2c1d9",
		Subject: promptlab.Subject{Role: "review"},
		Title:   "Prompt Lab: tighten instructions for role review",
		Evidence: promptlab.WeakSubject{
			Subject: promptlab.Subject{Role: "review"},
			Metric:  "failure_rate",
			Samples: 157,
		},
		Candidate:             promptlab.VariantCandidate{ID: "pl-a2d853b2c1d9", Intent: "tighten-instructions"},
		Offline:               promptlab.OfflineResult{Verdict: verdict},
		RequiresHumanApproval: verdict != promptlab.VerdictPassed,
	}
}

func promptLabCfg(autoApprove *bool) *config.Config {
	return &config.Config{
		PromptLab:  config.PromptLabConfig{Enabled: true, AutoApprove: autoApprove},
		Evaluation: config.EvaluationConfig{Offline: config.OfflineEvalConfig{Enabled: true}},
	}
}

// TestFileScrubbedProposals_NoOfflineScreenNoAutoApprove pins the hard gate:
// App.initWorkflowEngine only builds prompteval.Gate when
// evaluation.offline.enabled is set, and a nil evalGate means A/B enrollment
// is not screened at all. With the screen off, the human click is the only
// barrier left, so auto-approve must stay off no matter what the operator set
// prompt_lab.auto_approve to.
func TestFileScrubbedProposals_NoOfflineScreenNoAutoApprove(t *testing.T) {
	t.Parallel()
	on := true
	var approved []string
	cfg := &config.Config{
		PromptLab:  config.PromptLabConfig{Enabled: true, AutoApprove: &on},
		Evaluation: config.EvaluationConfig{Offline: config.OfflineEvalConfig{Enabled: false}},
	}
	c := setupPromptLabCoordinator(t, cfg, func(id string) error {
		approved = append(approved, id)
		return nil
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictNoVerdict)},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("approved = %v, want none: an unscreened variant must not reach production A/B", approved)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want 1", len(filed))
	}
	if filed[0].Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", filed[0].Status)
	}
}

// TestFileScrubbedProposals_AutoApproveClosesTheLoop is the core autonomy
// case: the stub evaluator can only ever return no-verdict, so without
// auto-approve every filed proposal parks in human-required forever.
func TestFileScrubbedProposals_AutoApproveClosesTheLoop(t *testing.T) {
	t.Parallel()
	var approved []string
	c := setupPromptLabCoordinator(t, promptLabCfg(nil), func(id string) error {
		approved = append(approved, id)
		return nil
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictNoVerdict)},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want 1", len(filed))
	}
	if len(approved) != 1 || approved[0] != filed[0].ID {
		t.Fatalf("approved = %v, want auto-approve of %s (default is on)", approved, filed[0].ID)
	}
}

func TestFileScrubbedProposals_AutoApproveDisabled(t *testing.T) {
	t.Parallel()
	off := false
	var approved []string
	c := setupPromptLabCoordinator(t, promptLabCfg(&off), func(id string) error {
		approved = append(approved, id)
		return nil
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictNoVerdict)},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("approved = %v, want none when auto_approve is off", approved)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want 1", len(filed))
	}
	if filed[0].Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required to await a human", filed[0].Status)
	}
}

// TestFileScrubbedProposals_NeverAutoApprovesFailedVerdict guards the carve-out
// that keeps auto-approve honest: a FAILED verdict is a real evaluator
// rejecting the candidate, not the absent-text placeholder.
func TestFileScrubbedProposals_NeverAutoApprovesFailedVerdict(t *testing.T) {
	t.Parallel()
	var approved []string
	c := setupPromptLabCoordinator(t, promptLabCfg(nil), func(id string) error {
		approved = append(approved, id)
		return nil
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictFailed)},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("approved = %v, want a failed verdict to stay with a human", approved)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want 1", len(filed))
	}
	if filed[0].Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", filed[0].Status)
	}
}

// TestFileScrubbedProposals_AutoApproveFailureKeepsTicking pins that a failed
// approval leaves the task safely parked for a human instead of aborting the
// rest of the tick.
func TestFileScrubbedProposals_AutoApproveFailureKeepsTicking(t *testing.T) {
	t.Parallel()
	c := setupPromptLabCoordinator(t, promptLabCfg(nil), func(string) error {
		return io.ErrUnexpectedEOF
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictNoVerdict)},
	})
	if err != nil {
		t.Fatalf("a failed auto-approve must not fail the tick: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want the proposal still filed", len(filed))
	}
	got, err := c.tasks.Get(filed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required so a human can still approve", got.Status)
	}
}

// TestFileScrubbedProposals_NoProjectSkipsAutoApprove pins the live-reproduced
// failure: with no project to assign, the authoring workflow starts and its
// agent immediately dies on "no project_id: refusing to start agent without
// isolated worktree", dumping the task back into human-required under a
// status_reason that hides why. Park it up front with an actionable one
// instead of laundering it through a guaranteed-failed dispatch.
func TestFileScrubbedProposals_NoProjectSkipsAutoApprove(t *testing.T) {
	t.Parallel()
	var approved []string
	c := newPromptLabCoordinatorForTest(t, promptLabCfg(nil), func(id string) error {
		approved = append(approved, id)
		return nil
	})

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{
		Proposals: []promptlab.Proposal{weakProposal(promptlab.VerdictNoVerdict)},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("approved = %v, want no dispatch that is certain to fail", approved)
	}
	if len(filed) != 1 {
		t.Fatalf("filed %d proposals, want 1", len(filed))
	}
	got, err := c.tasks.Get(filed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != promptLabNoProjectReason {
		t.Fatalf("StatusReason = %q, want the actionable no-project reason", got.StatusReason)
	}
}

// TestFileScrubbedProposals_ShutdownStopsAutoApprove pins that a cancellation
// arriving mid-tick stops the loop handing out FURTHER approvals. Each approve
// dispatches the authoring workflow synchronously and that runs worktree setup
// inline (~150s measured here: mise install + npm ci), and App.Shutdown waits
// on the ticker's goroutine — so this bounds a multi-proposal tick to one such
// block rather than one per proposal.
//
// It does NOT bound an already-started approve: approve takes no ctx and
// DispatchEvent is uncancellable, so an in-flight worktree setup still runs to
// completion. Cancelling before the first proposal would pass trivially
// without testing the loop at all, so cancel from inside the first approve.
func TestFileScrubbedProposals_ShutdownStopsAutoApprove(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var approved []string
	c := setupPromptLabCoordinator(t, promptLabCfg(nil), func(id string) error {
		approved = append(approved, id)
		cancel()
		return nil
	})

	first := weakProposal(promptlab.VerdictNoVerdict)
	second := weakProposal(promptlab.VerdictNoVerdict)
	second.ID = "pl-5cf660095cb8"
	second.Candidate.ID = second.ID
	second.Title = "Prompt Lab: restructure context for role review"

	filed, err := c.fileScrubbedProposals(ctx, promptlab.RunResult{
		Proposals: []promptlab.Proposal{first, second},
	})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(filed) != 2 {
		t.Fatalf("filed %d proposals, want both persisted for the next tick", len(filed))
	}
	if len(approved) != 1 {
		t.Fatalf("approved = %v, want only the pre-cancel proposal dispatched", approved)
	}
	got, err := c.tasks.Get(filed[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want the post-cancel proposal left for the next tick", got.Status)
	}
}

// TestFileScrubbedProposals_SkipsAlreadyFiledTerminalProposal is the
// four-duplicates regression at the filer level.
func TestFileScrubbedProposals_SkipsAlreadyFiledTerminalProposal(t *testing.T) {
	t.Parallel()
	c := setupPromptLabCoordinator(t, promptLabCfg(nil), func(string) error { return nil })
	p := weakProposal(promptlab.VerdictNoVerdict)

	done := task.StatusDone
	tags := []string{promptlab.ProposalTag, "role:review"}
	if _, err := c.tasks.CreateFull(p.Title, promptlab.RenderProposalBody(p), task.AgentModeHeadless, task.Update{
		Status: &done,
		Tags:   &tags,
	}); err != nil {
		t.Fatal(err)
	}

	filed, err := c.fileScrubbedProposals(context.Background(), promptlab.RunResult{Proposals: []promptlab.Proposal{p}})
	if err != nil {
		t.Fatalf("fileScrubbedProposals: %v", err)
	}
	if len(filed) != 0 {
		t.Fatalf("filed %d proposals, want 0 — %s was already filed and completed", len(filed), p.ID)
	}
}
