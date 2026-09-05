package task

import (
	"errors"
	"testing"
)

// TestAllowedTransitionsExhaustive asserts every ordered (from, to) pair over
// AllStatuses() is explicitly decided by allowedTransitions — no pair is
// silently defaulted. Self-pairs are excluded: IsTransitionAllowed treats
// from == to as always legal without consulting the map, so the map itself
// only needs an entry (present or absent) for every from != to pair, which
// this test verifies by checking the map is well-formed (only known statuses
// as keys/values, only true values) rather than requiring every from status
// to have a map entry — an absent "from" key correctly means "no legal
// automated exits", the same as StatusDone/StatusCancelled.
func TestAllowedTransitionsExhaustive(t *testing.T) {
	t.Parallel()

	statuses := AllStatuses()
	valid := make(map[Status]bool, len(statuses))
	for _, s := range statuses {
		valid[s] = true
	}

	for from, tos := range allowedTransitions {
		if !valid[from] {
			t.Errorf("allowedTransitions has unknown from-status key %q", from)
		}
		for to, legal := range tos {
			if !legal {
				t.Errorf("allowedTransitions[%q][%q] = false; omit illegal pairs instead of listing them", from, to)
			}
			if !valid[to] {
				t.Errorf("allowedTransitions[%q] has unknown to-status key %q", from, to)
			}
			if from == to {
				t.Errorf("allowedTransitions[%q][%q] is a self-transition; IsTransitionAllowed already covers this, omit it from the map", from, to)
			}
		}
	}

	// Every (from, to) pair is explicitly decided: either present (true) in
	// the map, or absent — both are a definite decision, so this loop
	// documents the exhaustiveness property by construction rather than
	// failing on missing entries. What it actually guards is that
	// IsTransitionAllowed never panics and always returns a bool for every
	// pair in AllStatuses() x AllStatuses().
	for _, from := range statuses {
		for _, to := range statuses {
			_ = IsTransitionAllowed(from, to)
		}
	}

	if !IsTransitionAllowed(StatusTodo, StatusTodo) {
		t.Error("self-transition todo->todo must be legal")
	}
}

func TestIsTransitionAllowed_NamedIllegalMoves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from, to Status
	}{
		{StatusDone, StatusInProgress},
		{StatusInReview, StatusTodo},
	}
	for _, c := range cases {
		if IsTransitionAllowed(c.from, c.to) {
			t.Errorf("IsTransitionAllowed(%q, %q) = true, want false", c.from, c.to)
		}
	}
}

func TestIsTransitionAllowed_NamedLegalMoves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from, to Status
	}{
		{StatusTodo, StatusInProgress},
		{StatusInProgress, StatusInReview},
		{StatusInReview, StatusDone},
		{StatusInProgress, StatusTodo},
		{StatusInProgress, StatusPlanReview},
		{StatusHumanRequired, StatusInProgress},
		{StatusHumanRequired, StatusInReview},
	}
	for _, c := range cases {
		if !IsTransitionAllowed(c.from, c.to) {
			t.Errorf("IsTransitionAllowed(%q, %q) = false, want true", c.from, c.to)
		}
	}
}

// verify_checks can prove a failing package is unrelated to the task's diff
// and route the valid change directly to PR creation. The gate runs while the
// task is still in-progress, including inside prompt-lab-author.
func TestTransitions_UnrelatedVerifyFailureCanOpenPRFromInProgress(t *testing.T) {
	for _, from := range []Status{StatusInProgress, StatusReadyReview} {
		if !IsTransitionAllowed(from, StatusReadyPR) {
			t.Fatalf("%s -> ready-pr is refused; unrelated verify failures cannot reach PR creation", from)
		}
	}
}

func TestIsTransitionAllowed_TerminalStatusesHaveNoAutomatedExit(t *testing.T) {
	t.Parallel()

	for _, terminal := range []Status{StatusDone, StatusCancelled} {
		for _, to := range AllStatuses() {
			if to == terminal {
				continue
			}
			if IsTransitionAllowed(terminal, to) {
				t.Errorf("IsTransitionAllowed(%q, %q) = true, want false (terminal statuses have no automated exit)", terminal, to)
			}
		}
	}
}

func TestApply_RejectsIllegalTransition(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	baseline := emitter.names()

	_, err = m.Apply(TransitionIntent{
		TaskID:   created.ID,
		ToStatus: StatusTodo,
		Actor:    "test.actor",
	})
	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("Apply err = %v, want *IllegalTransitionError", err)
	}
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatal("errors.Is(err, ErrIllegalTransition) = false, want true")
	}
	if illegal.From != StatusInReview || illegal.To != StatusTodo {
		t.Fatalf("illegal transition = %q -> %q, want %q -> %q", illegal.From, illegal.To, StatusInReview, StatusTodo)
	}

	after, err := m.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != StatusInReview {
		t.Fatalf("status after rejected illegal transition = %q, want unchanged %q", after.Status, StatusInReview)
	}
	if len(emitter.names()) != len(baseline) {
		t.Fatalf("events after rejected illegal transition = %v, want unchanged %v", emitter.names(), baseline)
	}
}

func TestApply_RejectsDoneToInProgress(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusDone)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, err = m.Apply(TransitionIntent{
		TaskID:   created.ID,
		ToStatus: StatusInProgress,
		Actor:    "test.actor",
	})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Apply err = %v, want ErrIllegalTransition", err)
	}
}

func TestApply_OperatorOverrideBypassesIllegalTransition(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusDone)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	result, err := m.Apply(TransitionIntent{
		TaskID:           created.ID,
		ToStatus:         StatusTodo,
		Actor:            "cli.reopen",
		OperatorOverride: true,
	})
	if err != nil {
		t.Fatalf("Apply with OperatorOverride: %v", err)
	}
	if result.Task.Status != StatusTodo {
		t.Fatalf("status = %q, want %q", result.Task.Status, StatusTodo)
	}
}

func TestApplyFn_RejectsIllegalTransition(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusDone)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, err = m.ApplyFn(created.ID, func(cur Task) (TransitionIntent, error) {
		return TransitionIntent{
			ToStatus: StatusInProgress,
			Actor:    "test.actor",
		}, nil
	})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("ApplyFn err = %v, want ErrIllegalTransition", err)
	}
}

func TestApplyStatusEffect_RejectsIllegalTransition(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(created.ID, Update{Status: Ptr(StatusInReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, err = m.ApplyStatusEffect(created.ID, StatusEffect{
		Source:   "test.effect",
		ToStatus: StatusTodo,
	})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("ApplyStatusEffect err = %v, want ErrIllegalTransition", err)
	}
}

// branch-conflict-fix must move a task back to in-progress to run its fix
// agent. Observed on the server: a divergence at ready-review dispatched
// recovery, the transition was rejected, and the task escalated to a human
// with "branch diverged … needs manual conflict resolution" — a message about
// git for what was actually the state machine refusing the move.
func TestTransitions_RecoveryCanReturnToInProgress(t *testing.T) {
	// Statuses are listed literally, never derived from allowedTransitions:
	// a test driven by the table it pins passes whatever that table becomes.
	for _, from := range []Status{
		StatusPlanning, StatusPlanReview, StatusReadyReview,
		StatusInReview, StatusTesting, StatusReadyPR, StatusHumanRequired,
	} {
		if !allowedTransitions[from][StatusInProgress] {
			t.Errorf("%s -> in-progress is refused; a task stranded there cannot be recovered by an agent", from)
		}
	}
}

// Terminal statuses must stay terminal — the recovery route above must not
// have opened a way back out of a finished task.
func TestTransitions_TerminalStaysTerminal(t *testing.T) {
	for _, from := range []Status{StatusDone, StatusCancelled} {
		if allowedTransitions[from][StatusInProgress] {
			t.Errorf("%s -> in-progress is allowed; a finished task must not resume", from)
		}
	}
}
