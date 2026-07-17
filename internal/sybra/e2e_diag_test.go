//go:build e2e

package sybra

import (
	"strings"
	"testing"
)

// The e2e hang (#2176) has never been root-caused because every occurrence
// destroys its own evidence: the app logger was DiscardHandler, so a CI-only
// timeout left nothing but "timeout waiting for X" and a goroutine dump in
// which every entry was a parked test. These two helpers are what make the next
// occurrence explain itself, so they are worth pinning.

func TestE2ELogBuffer_KeepsTheTailNotTheHead(t *testing.T) {
	t.Parallel()

	b := &e2eLogBuffer{}
	// The head is startup noise; the tail is what the app did before it hung.
	if _, err := b.Write([]byte(strings.Repeat("A", e2eLogTailBytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("THE-INTERESTING-PART")); err != nil {
		t.Fatal(err)
	}

	got := b.String()
	if len(got) > e2eLogTailBytes {
		t.Errorf("len = %d, want <= %d: the buffer must stay capped", len(got), e2eLogTailBytes)
	}
	if !strings.HasSuffix(got, "THE-INTERESTING-PART") {
		t.Error("the newest bytes were dropped; a hung run's last lines are the whole point")
	}
	if strings.HasPrefix(got, strings.Repeat("A", 64)) && len(got) == e2eLogTailBytes && !strings.Contains(got, "THE-INTERESTING-PART") {
		t.Error("buffer kept the head instead of the tail")
	}
}

func TestE2ELogBuffer_ShortWritesSurviveIntact(t *testing.T) {
	t.Parallel()

	b := &e2eLogBuffer{}
	if _, err := b.Write([]byte("agent.start\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("workflow.advance\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "agent.start\nworkflow.advance\n"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestDropParkedTestGoroutines(t *testing.T) {
	t.Parallel()

	// Shape mirrors a real dump: a blocked sequential test, an app goroutine,
	// and parked parallel tests that outnumber both.
	dump := strings.Join([]string{
		"goroutine 1 [chan receive, 2 minutes]:\ntesting.(*T).Run(0xc0001)\n\tmain.go:1 +0x1",
		"goroutine 52 [chan receive, 10 minutes]:\ntesting.(*T).Parallel(0xc0002)\n\ttesting.go:1803 +0x50c",
		"goroutine 77 [select]:\ngithub.com/Automaat/sybra/internal/agent.(*Manager).runHeadless(0xc0003)\n\trunner.go:1 +0x1",
		"goroutine 91 [chan receive, 10 minutes]:\ntesting.(*T).Parallel(0xc0004)\n\ttesting.go:1803 +0x50c",
	}, "\n\n")

	got := dropParkedTestGoroutines(dump)

	if strings.Contains(got, "testing.(*T).Parallel(") {
		t.Error("parked test goroutines survived; they are the noise that buried the culprit")
	}
	if !strings.Contains(got, "runHeadless") {
		t.Error("dropped an app goroutine — that is exactly what the dump exists to show")
	}
	if !strings.Contains(got, "testing.(*T).Run(") {
		t.Error("dropped the blocked sequential test's own stack")
	}
	if !strings.Contains(got, "2 goroutines parked in t.Parallel omitted") {
		t.Errorf("omission must be reported so the dump is not silently partial, got:\n%s", got)
	}
}

func TestDropParkedTestGoroutines_NothingToDrop(t *testing.T) {
	t.Parallel()

	dump := "goroutine 77 [select]:\ninternal/agent.(*Manager).runHeadless(0xc0003)\n\trunner.go:1 +0x1"

	got := dropParkedTestGoroutines(dump)
	if !strings.Contains(got, "runHeadless") {
		t.Errorf("dump = %q, want the goroutine kept", got)
	}
	if strings.Contains(got, "omitted") {
		t.Error("reported an omission when nothing was dropped")
	}
}
