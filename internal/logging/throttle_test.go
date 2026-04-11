package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func newTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

func countLines(buf *bytes.Buffer, substr string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func TestErrorThrottle_FirstErrorAtError(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	th.Log(logger, "todoist.import", "import", errors.New("dial: no host"))

	if got := countLines(buf, "level=ERROR"); got != 1 {
		t.Fatalf("ERROR lines = %d, want 1", got)
	}
}

func TestErrorThrottle_RepeatDowngradedToDebug(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	for range 5 {
		th.Log(logger, "todoist.import", "import", errors.New("dial: no host"))
	}

	if got := countLines(buf, "level=ERROR"); got != 1 {
		t.Errorf("ERROR lines = %d, want 1", got)
	}
	if got := countLines(buf, "level=DEBUG"); got != 4 {
		t.Errorf("DEBUG lines = %d, want 4", got)
	}
}

func TestErrorThrottle_DifferentErrorReArms(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	th.Log(logger, "todoist.import", "import", errors.New("dial: no host"))
	th.Log(logger, "todoist.import", "import", errors.New("dial: no host"))
	th.Log(logger, "todoist.import", "import", errors.New("HTTP 500"))

	if got := countLines(buf, "level=ERROR"); got != 2 {
		t.Errorf("ERROR lines = %d, want 2", got)
	}
}

func TestErrorThrottle_ClearReArms(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	err := errors.New("dial: no host")
	th.Log(logger, "todoist.import", "import", err)
	th.Clear("import")
	th.Log(logger, "todoist.import", "import", err)

	if got := countLines(buf, "level=ERROR"); got != 2 {
		t.Errorf("ERROR lines = %d, want 2", got)
	}
}

func TestErrorThrottle_NilErrorClears(t *testing.T) {
	t.Parallel()
	logger, _ := newTestLogger(t)
	th := NewErrorThrottle()

	th.Log(logger, "todoist.import", "import", errors.New("boom"))
	th.Log(logger, "todoist.import", "import", nil) // success: clears state
	th.mu.Lock()
	_, present := th.last["import"]
	th.mu.Unlock()
	if present {
		t.Error("expected entry to be cleared after nil err")
	}
}

func TestErrorThrottle_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	err := errors.New("same message")
	th.Log(logger, "op", "task-a", err)
	th.Log(logger, "op", "task-b", err)

	if got := countLines(buf, "level=ERROR"); got != 2 {
		t.Errorf("ERROR lines = %d, want 2 (one per key)", got)
	}
}
