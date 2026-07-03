package agentorch

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestPrependSupervisorSteer covers the consumption point that delivers a
// watchdog headless nudge: the steer is prepended to the next agent's prompt
// and cleared so it is applied exactly once.
func TestPrependSupervisorSteer(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	tk, err := tasks.Create("loop", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// No pending steer → prompt unchanged, no error.
	if got, err := PrependSupervisorSteer(tasks, tk.ID, "DO X"); err != nil || got != "DO X" {
		t.Fatalf("no-steer = (%q, %v), want (\"DO X\", nil)", got, err)
	}

	steer := "stop retrying; read the error first"
	if _, err := tasks.Update(tk.ID, task.Update{SupervisorSteer: &steer}); err != nil {
		t.Fatal(err)
	}

	got, err := PrependSupervisorSteer(tasks, tk.ID, "DO X")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "Supervisor course-correction: stop retrying; read the error first\n\nDO X"
	if got != want {
		t.Fatalf("steered prompt = %q, want %q", got, want)
	}

	// One-shot: the steer is cleared, so a second dispatch is unsteered.
	reread, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.SupervisorSteer != "" {
		t.Fatalf("steer not cleared after use: %q", reread.SupervisorSteer)
	}
	if again, err := PrependSupervisorSteer(tasks, tk.ID, "DO Y"); err != nil || again != "DO Y" {
		t.Fatalf("second call = (%q, %v), want (\"DO Y\", nil) — steer already consumed", again, err)
	}

	// Unknown task: prompt is returned unchanged and the read error surfaces so
	// the caller dispatches unsteered rather than silently swallowing it.
	if got, err := PrependSupervisorSteer(tasks, "does-not-exist", "DO Z"); err == nil || got != "DO Z" {
		t.Fatalf("unknown-task = (%q, %v), want (\"DO Z\", <error>)", got, err)
	}
}
