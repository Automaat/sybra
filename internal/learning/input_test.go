package learning

import (
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/stats"
)

func TestWindowForColdStart(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	since, until := windowFor(now, nil, 7, 30)
	if !until.Equal(now) {
		t.Fatalf("until = %v, want %v", until, now)
	}
	wantSince := now.AddDate(0, 0, -7)
	if !since.Equal(wantSince) {
		t.Fatalf("cold-start since = %v, want %v", since, wantSince)
	}
}

func TestWindowForContiguousFromPrevious(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	prevUntil := now.AddDate(0, 0, -2)
	prev := &Digest{Until: prevUntil}
	since, until := windowFor(now, prev, 7, 30)
	if !until.Equal(now) {
		t.Fatalf("until = %v, want %v", until, now)
	}
	if !since.Equal(prevUntil) {
		t.Fatalf("contiguous since = %v, want previous digest's until %v", since, prevUntil)
	}
}

func TestWindowForCappedAtMaxWindowDays(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	// Previous digest is 90 days stale (instance was down); span must clamp to
	// maxWindowDays rather than feeding the summarizer an unbounded window.
	prev := &Digest{Until: now.AddDate(0, 0, -90)}
	since, until := windowFor(now, prev, 7, 30)
	wantSince := now.AddDate(0, 0, -30)
	if !since.Equal(wantSince) {
		t.Fatalf("capped since = %v, want %v", since, wantSince)
	}
	if !until.Equal(now) {
		t.Fatalf("until = %v, want %v", until, now)
	}
}

func TestFreshSignalCountsWithinWindow(t *testing.T) {
	since := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	in := since.Add(time.Hour)
	before := since.Add(-time.Hour)
	after := until.Add(time.Hour)

	recs := []stats.RunRecord{
		{ID: "in-window", Timestamp: in},
		{ID: "before-window", Timestamp: before},
		{ID: "after-window", Timestamp: after},
	}
	evts := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in},
		{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: before},
		{Type: audit.EventAgentCompleted, TaskID: "C", Timestamp: in}, // wrong type, must not count
	}

	fresh, landed := freshSignal(recs, evts, since, until)
	if fresh != 1 {
		t.Fatalf("freshRuns = %d, want 1", fresh)
	}
	if landed != 1 {
		t.Fatalf("landed = %d, want 1", landed)
	}
}

func TestBuildPacketHashDeterministicAndDedupFriendly(t *testing.T) {
	since := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	recs := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", Outcome: "completed", Timestamp: since.Add(time.Hour)},
	}
	evts := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: since.Add(time.Hour), Data: map[string]any{"outcome": "merged"}},
	}
	abTesting := abtest.Config{MinSamplesPerVariant: 20}

	p1 := buildPacket(recs, evts, abTesting, since, until, nil)
	p2 := buildPacket(recs, evts, abTesting, since, until, nil)
	if p1.ReportDigest != p2.ReportDigest {
		t.Fatalf("ReportDigest not deterministic: %q != %q", p1.ReportDigest, p2.ReportDigest)
	}
	if p1.ReportDigest == "" {
		t.Fatal("ReportDigest must not be empty")
	}

	// Different underlying data must hash differently, so a genuine change in
	// the window's signal produces a new dedup identity.
	recs2 := append(append([]stats.RunRecord{}, recs...), stats.RunRecord{
		TaskID: "B", Role: "implementation", Provider: "codex", Outcome: "failed", Timestamp: since.Add(2 * time.Hour),
	})
	p3 := buildPacket(recs2, evts, abTesting, since, until, nil)
	if p3.ReportDigest == p1.ReportDigest {
		t.Fatal("different underlying data must produce a different ReportDigest")
	}
}

func TestBuildPacketCarriesPreviousNextBets(t *testing.T) {
	since := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	prev := &Digest{NextBets: []string{"try skill variant B"}}

	pkt := buildPacket(nil, nil, abtest.Config{}, since, until, prev)
	if len(pkt.PreviousNextBets) != 1 || pkt.PreviousNextBets[0] != "try skill variant B" {
		t.Fatalf("PreviousNextBets = %+v, want [\"try skill variant B\"]", pkt.PreviousNextBets)
	}
}

func TestBuildPacketExperimentSignalsCarryInsufficientData(t *testing.T) {
	since := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	in := since.Add(time.Hour)

	recs := []stats.RunRecord{
		{
			TaskID:           "A",
			Role:             "implementation",
			Provider:         "claude",
			Model:            "sonnet",
			ExperimentID:     "exp",
			VariantID:        "a",
			SkillConformance: skillattr.ConformanceNone,
			Outcome:          "completed",
			Timestamp:        in,
		},
	}
	evts := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
	}
	abTesting := abtest.Config{
		MinSamplesPerVariant: 20,
		Experiments: []abtest.Experiment{
			{ID: "exp", Kind: "model", Variants: []abtest.Variant{{ID: "a"}}},
		},
	}

	pkt := buildPacket(recs, evts, abTesting, since, until, nil)
	if len(pkt.Experiments) != 1 {
		t.Fatalf("Experiments = %+v, want 1 row", pkt.Experiments)
	}
	row := pkt.Experiments[0]
	if row.ExperimentID != "exp" || row.VariantID != "a" || row.Kind != "model" {
		t.Fatalf("experiment row = %+v, want exp/a/model", row)
	}
	if !row.InsufficientData {
		t.Fatalf("row with 1 run against MinSamplesPerVariant=20 must be flagged InsufficientData: %+v", row)
	}
	if row.SampleStatus != evaluation.SampleStatusLowSample {
		t.Fatalf("row sample status = %q, want %q", row.SampleStatus, evaluation.SampleStatusLowSample)
	}
}

func TestBuildPacketExperimentSignalsCarryParityUnknownStatus(t *testing.T) {
	since := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	in := since.Add(time.Hour)

	recs := []stats.RunRecord{
		{
			TaskID:           "A",
			Role:             "implementation",
			Provider:         "claude",
			Model:            "sonnet",
			ExperimentID:     "exp",
			VariantID:        "a",
			SkillConformance: skillattr.ConformanceUnverified,
			Outcome:          "completed",
			Timestamp:        in,
		},
	}
	abTesting := abtest.Config{
		MinSamplesPerVariant: 1,
		Experiments: []abtest.Experiment{
			{ID: "exp", Kind: "model", Variants: []abtest.Variant{{ID: "a"}}},
		},
	}

	pkt := buildPacket(recs, nil, abTesting, since, until, nil)
	if len(pkt.Experiments) != 1 {
		t.Fatalf("Experiments = %+v, want 1 row", pkt.Experiments)
	}
	row := pkt.Experiments[0]
	if !row.InsufficientData {
		t.Fatalf("row with parity-unknown delivery must be flagged InsufficientData: %+v", row)
	}
	if row.SampleStatus != evaluation.SampleStatusParityUnknown {
		t.Fatalf("row sample status = %q, want %q", row.SampleStatus, evaluation.SampleStatusParityUnknown)
	}
}

func TestBuildPromptExplainsParityUnknownExperimentSignals(t *testing.T) {
	prompt := buildPrompt(Packet{
		Since: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
		Experiments: []ExperimentSignal{{
			ExperimentID:     "exp",
			VariantID:        "control",
			Kind:             "model",
			InsufficientData: true,
			SampleStatus:     evaluation.SampleStatusParityUnknown,
		}},
	})
	if !strings.Contains(prompt, "parity-unknown") {
		t.Fatalf("prompt missing parity-unknown guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "never frame parity-unknown rows as wins/losses") {
		t.Fatalf("prompt missing parity caveat:\n%s", prompt)
	}
}
