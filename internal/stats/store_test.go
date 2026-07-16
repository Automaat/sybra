package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewStoreEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("expected 0 runs, got %d", s.Len())
	}
}

func TestRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	runs := []RunRecord{
		{
			ID: "a1", TaskID: "t1", ProjectID: "org/repo",
			Mode: "headless", Role: "implementation", Model: "sonnet",
			RequestedSkill: "sybra-test", SkillExecutionMode: "native", SkillConformance: "exact",
			CostUSD: 0.05, DurationS: 30, InputTokens: 1000, OutputTokens: 500,
			Outcome: "completed", Timestamp: now,
		},
		{
			ID: "a2", TaskID: "t2", ProjectID: "org/repo",
			Mode: "interactive", Role: "triage", Model: "opus",
			SkillExecutionMode: "none", SkillConformance: "none",
			CostUSD: 0.10, DurationS: 60, InputTokens: 2000, OutputTokens: 1000,
			Outcome: "completed", Timestamp: now.Add(-time.Hour),
		},
		{
			ID: "a3", TaskID: "t3", ProjectID: "org/other",
			Mode: "headless", Role: "plan", Model: "sonnet",
			// Legacy blank skill metadata must stay readable and group as unknown.
			CostUSD: 0.03, DurationS: 20, InputTokens: 500, OutputTokens: 200,
			Outcome: "failed", Timestamp: now.Add(-48 * time.Hour),
		},
	}

	for _, r := range runs {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	if s.Len() != 3 {
		t.Fatalf("expected 3 runs, got %d", s.Len())
	}

	resp := s.Query()

	// AllTime
	if resp.AllTime.TotalRuns != 3 {
		t.Errorf("allTime.totalRuns: got %d, want 3", resp.AllTime.TotalRuns)
	}
	wantCost := 0.05 + 0.10 + 0.03
	if diff := resp.AllTime.TotalCostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("allTime.totalCost: got %f, want %f", resp.AllTime.TotalCostUSD, wantCost)
	}
	if resp.AllTime.TotalInputTokens != 3500 {
		t.Errorf("allTime.totalInputTokens: got %d, want 3500", resp.AllTime.TotalInputTokens)
	}
	if resp.AllTime.FailedRuns != 1 {
		t.Errorf("allTime.failedRuns: got %d, want 1", resp.AllTime.FailedRuns)
	}
	if got := resp.AllTime.OutcomeCounts["completed"]; got != 2 {
		t.Errorf("allTime.outcomeCounts[completed]: got %d, want 2", got)
	}
	if got := resp.AllTime.OutcomeCounts["failed"]; got != 1 {
		t.Errorf("allTime.outcomeCounts[failed]: got %d, want 1", got)
	}

	// ByProject sorted by cost desc
	if len(resp.ByProject) != 2 {
		t.Fatalf("byProject: got %d groups, want 2", len(resp.ByProject))
	}
	if resp.ByProject[0].Key != "org/repo" {
		t.Errorf("byProject[0].key: got %s, want org/repo", resp.ByProject[0].Key)
	}

	// ByMode
	if len(resp.ByMode) != 2 {
		t.Errorf("byMode: got %d groups, want 2", len(resp.ByMode))
	}
	if len(resp.BySkillExecutionMode) != 3 {
		t.Fatalf("bySkillExecutionMode: got %d groups, want 3", len(resp.BySkillExecutionMode))
	}
	if resp.BySkillExecutionMode[0].Key != "none" {
		t.Errorf("bySkillExecutionMode[0].Key = %q, want none", resp.BySkillExecutionMode[0].Key)
	}
	if resp.BySkillExecutionMode[1].Key != "native" {
		t.Errorf("bySkillExecutionMode[1].Key = %q, want native", resp.BySkillExecutionMode[1].Key)
	}
	if resp.BySkillExecutionMode[2].Key != "unknown" {
		t.Errorf("bySkillExecutionMode[2].Key = %q, want unknown", resp.BySkillExecutionMode[2].Key)
	}
	unknown := resp.BySkillExecutionMode[2].Stats
	if unknown.FailedRuns != 1 {
		t.Errorf("unknown.failedRuns = %d, want 1", unknown.FailedRuns)
	}
	if got := unknown.OutcomeCounts["failed"]; got != 1 {
		t.Errorf("unknown.outcomeCounts[failed] = %d, want 1", got)
	}

	// RecentRuns newest first
	if len(resp.RecentRuns) != 3 {
		t.Fatalf("recentRuns: got %d, want 3", len(resp.RecentRuns))
	}
	if resp.RecentRuns[0].ID != "a1" {
		t.Errorf("recentRuns[0].id: got %s, want a1", resp.RecentRuns[0].ID)
	}
	if resp.RecentRuns[2].SkillExecutionMode != "unknown" {
		t.Errorf("recentRuns[2].SkillExecutionMode = %q, want unknown", resp.RecentRuns[2].SkillExecutionMode)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	r := RunRecord{
		ID: "a1", TaskID: "t1", Mode: "headless", Role: "implementation",
		CostUSD: 0.05, DurationS: 30, Outcome: "completed",
		Timestamp: time.Now(),
	}
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatal("stats file not created")
	}

	// Reload from disk
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 1 {
		t.Fatalf("after reload: expected 1 run, got %d", s2.Len())
	}

	resp := s2.Query()
	if resp.AllTime.TotalCostUSD != 0.05 {
		t.Errorf("after reload: totalCost got %f, want 0.05", resp.AllTime.TotalCostUSD)
	}
}

// TestRecordCrossProcessSimulatesConcurrentWriters models two OS processes
// (e.g. the GUI server and sybra-cli) each holding their own *Store over the
// same path. s.runs is loaded once at NewStore time and never re-read on the
// query paths, so without Record reloading from disk under the flock before
// appending, s2.Record would overwrite s1's not-yet-visible-to-s2 run
// entirely instead of merging with it.
func TestRecordCrossProcessSimulatesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s1.Record(RunRecord{ID: "a1", TaskID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("s1.Record: %v", err)
	}
	if err := s2.Record(RunRecord{ID: "a2", TaskID: "t2", Timestamp: time.Now()}); err != nil {
		t.Fatalf("s2.Record: %v", err)
	}

	s3, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 — a run was dropped by concurrent cross-process writes", s3.Len())
	}
}

func TestReasoningTokensPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Record(RunRecord{
		ID: "r1", TaskID: "t1", Mode: "headless", Role: "implementation",
		ReasoningTokens: 300, Outcome: "completed", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Reload and verify field survives the round-trip.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	resp := s2.Query()
	if len(resp.RecentRuns) != 1 {
		t.Fatalf("expected 1 run after reload, got %d", len(resp.RecentRuns))
	}
	if resp.RecentRuns[0].ReasoningTokens != 300 {
		t.Errorf("ReasoningTokens after reload: got %d, want 300", resp.RecentRuns[0].ReasoningTokens)
	}
	if resp.AllTime.TotalReasoningTokens != 300 {
		t.Errorf("TotalReasoningTokens: got %d, want 300", resp.AllTime.TotalReasoningTokens)
	}
}

func TestQueryEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	resp := s.Query()
	if resp.AllTime.TotalRuns != 0 {
		t.Errorf("empty store: expected 0 runs, got %d", resp.AllTime.TotalRuns)
	}
	if len(resp.RecentRuns) != 0 {
		t.Errorf("empty store: expected empty recentRuns, got %d", len(resp.RecentRuns))
	}
}

func TestReviewRoundsByModel(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	at := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Minute) }

	runs := []RunRecord{
		// opus task: clean first pass.
		{TaskID: "t1", Role: "implementation", Model: "opus", Timestamp: at(0)},
		{TaskID: "t1", Role: "review", Model: "sonnet", Timestamp: at(1)},

		// opus task: needed three review rounds.
		{TaskID: "t2", Role: "implementation", Model: "opus", Timestamp: at(0)},
		{TaskID: "t2", Role: "review", Model: "sonnet", Timestamp: at(1)},
		{TaskID: "t2", Role: "fix-review", Model: "sonnet", Timestamp: at(2)},
		{TaskID: "t2", Role: "review", Model: "sonnet", Timestamp: at(3)},
		{TaskID: "t2", Role: "fix-review", Model: "sonnet", Timestamp: at(4)},
		{TaskID: "t2", Role: "review", Model: "sonnet", Timestamp: at(5)},

		// legacy empty role counts as implementation.
		{TaskID: "t3", Role: "", Model: "codex", Timestamp: at(0)},
		{TaskID: "t3", Role: "review", Model: "sonnet", Timestamp: at(1)},

		// never reviewed -> excluded entirely.
		{TaskID: "t4", Role: "implementation", Model: "opus", Timestamp: at(0)},

		// failover mid-task: attributed to earliest impl, flagged mixed.
		{TaskID: "t5", Role: "implementation", Model: "codex", Timestamp: at(9)},
		{TaskID: "t5", Role: "implementation", Model: "opus", Timestamp: at(0)},
		{TaskID: "t5", Role: "review", Model: "sonnet", Timestamp: at(10)},
	}

	got := reviewRoundsByModel(runs)

	byKey := map[string]ReviewRoundsStat{}
	for _, s := range got {
		byKey[s.Key] = s
	}

	opus := byKey["opus"]
	if opus.Tasks != 3 {
		t.Errorf("opus Tasks = %d, want 3 (t4 unreviewed must be excluded)", opus.Tasks)
	}
	if opus.TotalRounds != 5 {
		t.Errorf("opus TotalRounds = %d, want 5", opus.TotalRounds)
	}
	if opus.MaxRounds != 3 {
		t.Errorf("opus MaxRounds = %d, want 3", opus.MaxRounds)
	}
	if opus.CleanFirstPass != 2 {
		t.Errorf("opus CleanFirstPass = %d, want 2", opus.CleanFirstPass)
	}
	if opus.MixedImplModels != 1 {
		t.Errorf("opus MixedImplModels = %d, want 1 (t5 failed over)", opus.MixedImplModels)
	}
	if opus.AvgRounds < 1.66 || opus.AvgRounds > 1.67 {
		t.Errorf("opus AvgRounds = %v, want ~1.667", opus.AvgRounds)
	}

	codex := byKey["codex"]
	if codex.Tasks != 1 || codex.TotalRounds != 1 {
		t.Errorf("codex = %+v, want 1 task / 1 round (empty role is implementation)", codex)
	}

	if _, ok := byKey["(unknown)"]; ok {
		t.Errorf("unexpected (unknown) bucket: %+v", got)
	}
}

func TestReviewRoundsByModel_ExcludesUnreviewedAndEmptyTaskID(t *testing.T) {
	t.Parallel()

	runs := []RunRecord{
		{TaskID: "", Role: "implementation", Model: "opus"},
		{TaskID: "", Role: "review", Model: "sonnet"},
		{TaskID: "t1", Role: "review", Model: "sonnet"},
	}
	if got := reviewRoundsByModel(runs); len(got) != 0 {
		t.Errorf("reviewRoundsByModel = %+v, want empty (no attributable impl run)", got)
	}
}
