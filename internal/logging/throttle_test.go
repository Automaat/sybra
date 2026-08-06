package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
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

	th.Log(logger, "poller.import", "import", errors.New("dial: no host"))

	if got := countLines(buf, "level=ERROR"); got != 1 {
		t.Fatalf("ERROR lines = %d, want 1", got)
	}
}

func TestErrorThrottle_RepeatDowngradedToDebug(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	for range 5 {
		th.Log(logger, "poller.import", "import", errors.New("dial: no host"))
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

	th.Log(logger, "poller.import", "import", errors.New("dial: no host"))
	th.Log(logger, "poller.import", "import", errors.New("dial: no host"))
	th.Log(logger, "poller.import", "import", errors.New("HTTP 500"))

	if got := countLines(buf, "level=ERROR"); got != 2 {
		t.Errorf("ERROR lines = %d, want 2", got)
	}
}

func TestErrorThrottle_ClearReArms(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewErrorThrottle()

	err := errors.New("dial: no host")
	th.Log(logger, "poller.import", "import", err)
	th.Clear("import")
	th.Log(logger, "poller.import", "import", err)

	if got := countLines(buf, "level=ERROR"); got != 2 {
		t.Errorf("ERROR lines = %d, want 2", got)
	}
}

func TestErrorThrottle_NilErrorClears(t *testing.T) {
	t.Parallel()
	logger, _ := newTestLogger(t)
	th := NewErrorThrottle()

	th.Log(logger, "poller.import", "import", errors.New("boom"))
	th.Log(logger, "poller.import", "import", nil) // success: clears state
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

func TestInfoThrottle_FirstOccurrenceAtInfo(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")

	if got := countLines(buf, "level=INFO"); got != 1 {
		t.Fatalf("INFO lines = %d, want 1", got)
	}
}

// A provider park can now last days, so downgrading a repeat to DEBUG is not
// enough on its own — at the 60s maintenance interval a 60-hour park writes
// one line per tick at debug level. The interval is what bounds the volume.
func TestInfoThrottle_LongParkDoesNotLogPerTick(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	clock := time.Now()
	th.now = func() time.Time { return clock }

	const park = 60 * time.Hour
	const tick = time.Minute
	ticks := 0
	for elapsed := time.Duration(0); elapsed < park; elapsed += tick {
		th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")
		clock = clock.Add(tick)
		ticks++
	}

	info := countLines(buf, "level=INFO")
	debug := countLines(buf, "level=DEBUG")
	if info != 1 {
		t.Errorf("INFO lines = %d, want 1 (the park starting)", info)
	}
	// Literal, not derived from InfoRepeatInterval: pinning a constant to
	// itself lets any value pass, including one longer than the park.
	if debug != 119 {
		t.Errorf("DEBUG lines = %d, want 119 (60h at one re-emission per 30m, less the INFO tick)", debug)
	}
	if total := info + debug; total >= ticks {
		t.Errorf("%d lines for %d ticks: repeats are not being suppressed", total, ticks)
	}
}

// A state change must not wait out the interval: the whole point of logging a
// park is knowing when it moves or ends.
func TestInfoThrottle_ChangedValueReArmsImmediately(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	clock := time.Now()
	th.now = func() time.Time { return clock }

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")
	clock = clock.Add(time.Minute)
	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:claude")

	if got := countLines(buf, "level=INFO"); got != 2 {
		t.Fatalf("INFO lines = %d, want 2 (one per distinct value)", got)
	}
}

func TestInfoThrottle_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")
	th.Log(logger, "workflow.resume-stalled.skip", "t2", "provider_rate_limited:codex")

	if got := countLines(buf, "level=INFO"); got != 2 {
		t.Fatalf("INFO lines = %d, want 2 (one per task)", got)
	}
}

func TestInfoThrottle_ClearReArms(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")
	th.Clear("t1")
	th.Log(logger, "workflow.resume-stalled.skip", "t1", "provider_rate_limited:codex")

	if got := countLines(buf, "level=INFO"); got != 2 {
		t.Fatalf("INFO lines = %d, want 2 (Clear re-arms)", got)
	}
}

// Several call sites log different messages under the same task id. Keying on
// the id alone let whichever ran first suppress the others as repeats of
// itself, so one of two genuinely different lines vanished from the log.
func TestInfoThrottle_MessagesDoNotSuppressEachOther(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "claimed|implement")
	th.Log(logger, "workflow.effect-replay.skip", "t1", "claimed|implement")

	if got := countLines(buf, "level=INFO"); got != 2 {
		t.Fatalf("INFO lines = %d, want 2 (one per message)", got)
	}
	if got := countLines(buf, "workflow.effect-replay.skip"); got != 1 {
		t.Errorf("second message logged %d times, want 1", got)
	}
}

// Clear means "the condition ended", which is true of every reason the caller
// might have logged for that key — not just the last one.
func TestInfoThrottle_ClearDropsEveryMessage(t *testing.T) {
	t.Parallel()
	logger, buf := newTestLogger(t)
	th := NewInfoThrottle()

	th.Log(logger, "workflow.resume-stalled.skip", "t1", "claimed|implement")
	th.Log(logger, "workflow.effect-replay.skip", "t1", "claimed|implement")
	th.Clear("t1")
	th.Log(logger, "workflow.resume-stalled.skip", "t1", "claimed|implement")
	th.Log(logger, "workflow.effect-replay.skip", "t1", "claimed|implement")

	if got := countLines(buf, "level=INFO"); got != 4 {
		t.Fatalf("INFO lines = %d, want 4 (Clear re-arms both messages)", got)
	}
}
