package agent

import "testing"

func TestToolSignature(t *testing.T) {
	bash := func(cmd string) ToolUseBlock {
		return ToolUseBlock{Name: "Bash", Input: map[string]any{"command": cmd}}
	}
	read := func(path string, offset, limit int) ToolUseBlock {
		return ToolUseBlock{Name: "Read", Input: map[string]any{
			"file_path": path,
			"offset":    offset,
			"limit":     limit,
		}}
	}

	t.Run("empty for no tools", func(t *testing.T) {
		if got := toolSignature(nil); got != "" {
			t.Fatalf("toolSignature(nil) = %q, want empty", got)
		}
	})

	t.Run("identical semantic calls share a signature", func(t *testing.T) {
		a := toolSignature([]ToolUseBlock{bash("mise run verify 2>&1 | tail -n 200")})
		b := toolSignature([]ToolUseBlock{bash("mise run verify 2>&1 | tail -n 20")})
		if a == "" || a != b {
			t.Fatalf("identical calls: %q vs %q, want equal non-empty", a, b)
		}
	})

	t.Run("different command families differ", func(t *testing.T) {
		if toolSignature([]ToolUseBlock{bash("go test ./...")}) == toolSignature([]ToolUseBlock{bash("pwd")}) {
			t.Fatal("different commands hashed to the same signature")
		}
	})

	t.Run("equivalent read tools can converge semantically", func(t *testing.T) {
		a := toolSignature([]ToolUseBlock{bash("cat app.log | tail -n 20")})
		b := toolSignature([]ToolUseBlock{read("app.log", 200, 20)})
		if a == "" || a != b {
			t.Fatalf("read signatures = %q vs %q, want equal non-empty", a, b)
		}
	})

	t.Run("read ranges collapse by path", func(t *testing.T) {
		a := toolSignature([]ToolUseBlock{read("internal/watchdog/agent.go", 0, 200)})
		b := toolSignature([]ToolUseBlock{read("internal/watchdog/agent.go", 200, 200)})
		if a == "" || a != b {
			t.Fatalf("read signatures = %q vs %q, want equal non-empty", a, b)
		}
	})

	t.Run("order independent within an event", func(t *testing.T) {
		ab := toolSignature([]ToolUseBlock{bash("a"), bash("b")})
		ba := toolSignature([]ToolUseBlock{bash("b"), bash("a")})
		if ab != ba {
			t.Fatalf("order changed signature: %q vs %q", ab, ba)
		}
	})
}

func TestAgentNoteToolSignature(t *testing.T) {
	t.Run("repeats accumulate, new signature resets", func(t *testing.T) {
		a := &Agent{}
		if got := a.NoteToolSignature("x"); got != 1 {
			t.Fatalf("first note streak = %d, want 1", got)
		}
		if got := a.NoteToolSignature("x"); got != 2 {
			t.Fatalf("second note streak = %d, want 2", got)
		}
		if got := a.NoteToolSignature("x"); got != 3 {
			t.Fatalf("third note streak = %d, want 3", got)
		}
		if got := a.NoteToolSignature("y"); got != 1 {
			t.Fatalf("new signature streak = %d, want 1", got)
		}
		if got := a.ToolLoopStreak(); got != 1 {
			t.Fatalf("ToolLoopStreak = %d, want 1", got)
		}
	})

	t.Run("empty signature preserves the streak", func(t *testing.T) {
		a := &Agent{}
		a.NoteToolSignature("x")
		a.NoteToolSignature("x") // streak 2
		if got := a.NoteToolSignature(""); got != 2 {
			t.Fatalf("empty note streak = %d, want 2 (unchanged)", got)
		}
		if got := a.NoteToolSignature("x"); got != 3 {
			t.Fatalf("resumed streak = %d, want 3 (interleaved reasoning did not reset)", got)
		}
	})

	t.Run("two-family low-progress cycle accumulates over the window", func(t *testing.T) {
		a := &Agent{}
		wantScores := []int{1, 1, 1, 4, 5, 6}
		for i, sig := range []string{"build", "read", "build", "read", "build", "read"} {
			if got := a.NoteToolAction(sig, sig); got != wantScores[i] {
				t.Fatalf("note %d score = %d, want %d", i, got, wantScores[i])
			}
		}
		ev := a.ToolLoopEvidence()
		if ev.UniqueFamilies != 2 {
			t.Fatalf("UniqueFamilies = %d, want 2", ev.UniqueFamilies)
		}
		if ev.Count != 6 || ev.Window != 6 {
			t.Fatalf("evidence = %+v, want count/window 6", ev)
		}
	})

	t.Run("streak never exceeds the retained window", func(t *testing.T) {
		a := &Agent{}
		for range loopWindowSize + 8 {
			a.NoteToolSignature("x")
		}
		ev := a.ToolLoopEvidence()
		if ev.Count > ev.Window {
			t.Fatalf("evidence = %+v, Count must never exceed Window", ev)
		}
		if ev.Count != loopWindowSize || ev.Window != loopWindowSize {
			t.Fatalf("evidence = %+v, want count/window capped at %d", ev, loopWindowSize)
		}
	})

	t.Run("successful progress resets the low-progress window", func(t *testing.T) {
		a := &Agent{}
		a.NoteToolAction("check:go test ./...", "check:go test ./...")
		a.NoteToolAction("check:go test ./...", "check:go test ./...")
		a.NoteToolProgress()
		if got := a.ToolLoopStreak(); got != 0 {
			t.Fatalf("ToolLoopStreak after progress = %d, want 0", got)
		}
		if got := a.NoteToolAction("check:go test ./...", "check:go test ./..."); got != 1 {
			t.Fatalf("new post-progress score = %d, want 1", got)
		}
	})
}

func TestAgentToolLoopAcknowledge(t *testing.T) {
	t.Run("ack suppresses the same signature until it changes", func(t *testing.T) {
		a := &Agent{}
		a.NoteToolSignature("x")
		a.NoteToolSignature("x")
		if a.ToolLoopAcknowledged() {
			t.Fatal("unacknowledged before AckToolLoop")
		}
		a.AckToolLoop()
		if !a.ToolLoopAcknowledged() {
			t.Fatal("want acknowledged after AckToolLoop on same signature")
		}
		// Same signature keeps climbing but stays acknowledged.
		a.NoteToolSignature("x")
		if !a.ToolLoopAcknowledged() {
			t.Fatal("same signature must stay acknowledged")
		}
		// A new loop signature re-arms the trigger.
		a.NoteToolSignature("y")
		if a.ToolLoopAcknowledged() {
			t.Fatal("a different signature must clear the acknowledgement")
		}
	})

	t.Run("ack on no signature is not acknowledged", func(t *testing.T) {
		a := &Agent{}
		a.AckToolLoop() // no current signature must not count as acknowledged
		if a.ToolLoopAcknowledged() {
			t.Fatal("empty signature must never read as acknowledged")
		}
	})
}

func TestApplyStreamEventState_ResetsLoopWindowOnSuccessfulEditResult(t *testing.T) {
	a := &Agent{}
	a.NoteToolAction("check:go test ./...", "check:go test ./...")
	a.NoteToolAction("check:go test ./...", "check:go test ./...")
	a.applyStreamEventState(StreamEvent{
		toolUses: []ToolUseBlock{{
			ID:    "tu-1",
			Name:  "Write",
			Input: map[string]any{"file_path": "internal/watchdog/agent.go"},
		}},
	})
	a.applyStreamEventState(StreamEvent{
		toolResults: []ToolResultBlock{{
			ToolUseID: "tu-1",
			Content:   "ok",
		}},
	})
	if got := a.ToolLoopStreak(); got != 0 {
		t.Fatalf("ToolLoopStreak after successful edit = %d, want 0", got)
	}
}
