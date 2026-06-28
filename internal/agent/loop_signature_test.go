package agent

import "testing"

func TestToolSignature(t *testing.T) {
	bash := func(cmd string) ToolUseBlock {
		return ToolUseBlock{Name: "Bash", Input: map[string]any{"command": cmd}}
	}

	t.Run("empty for no tools", func(t *testing.T) {
		if got := toolSignature(nil); got != "" {
			t.Fatalf("toolSignature(nil) = %q, want empty", got)
		}
	})

	t.Run("identical calls share a signature", func(t *testing.T) {
		a := toolSignature([]ToolUseBlock{bash("ls -la")})
		b := toolSignature([]ToolUseBlock{bash("ls -la")})
		if a == "" || a != b {
			t.Fatalf("identical calls: %q vs %q, want equal non-empty", a, b)
		}
	})

	t.Run("different input differs", func(t *testing.T) {
		if toolSignature([]ToolUseBlock{bash("ls")}) == toolSignature([]ToolUseBlock{bash("pwd")}) {
			t.Fatal("different commands hashed to the same signature")
		}
	})

	t.Run("different tool name differs", func(t *testing.T) {
		read := ToolUseBlock{Name: "Read", Input: map[string]any{"command": "ls"}}
		if toolSignature([]ToolUseBlock{bash("ls")}) == toolSignature([]ToolUseBlock{read}) {
			t.Fatal("different tool names hashed to the same signature")
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
