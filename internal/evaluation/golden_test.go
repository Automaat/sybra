package evaluation

import (
	"path/filepath"
	"testing"
)

func TestScoreCase(t *testing.T) {
	c := GoldenCase{ID: "c1", Expect: Expectation{TestsPass: true, MinQuality: 7, MaxTurns: 30}}
	tests := []struct {
		name      string
		r         CaseResult
		wantPass  bool
		wantFails int
	}{
		{"all good", CaseResult{CaseID: "c1", TestsPass: true, Quality: 8, Turns: 20}, true, 0},
		{"tests fail", CaseResult{CaseID: "c1", TestsPass: false, Quality: 8, Turns: 20}, false, 1},
		{"low quality", CaseResult{CaseID: "c1", TestsPass: true, Quality: 5, Turns: 20}, false, 1},
		{"too many turns", CaseResult{CaseID: "c1", TestsPass: true, Quality: 8, Turns: 40}, false, 1},
		{"multiple failures", CaseResult{CaseID: "c1", TestsPass: false, Quality: 1, Turns: 99}, false, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oc := ScoreCase(c, tt.r)
			if oc.Passed != tt.wantPass {
				t.Errorf("Passed = %v, want %v (%v)", oc.Passed, tt.wantPass, oc.Failures)
			}
			if len(oc.Failures) != tt.wantFails {
				t.Errorf("Failures = %d, want %d: %v", len(oc.Failures), tt.wantFails, oc.Failures)
			}
		})
	}
}

func TestScoreCase_UncheckedFields(t *testing.T) {
	// Zero expectations mean "unchecked": only TestsPass matters here.
	c := GoldenCase{ID: "c", Expect: Expectation{TestsPass: true}}
	oc := ScoreCase(c, CaseResult{CaseID: "c", TestsPass: true, Quality: 0, Turns: 9999})
	if !oc.Passed {
		t.Errorf("unchecked quality/turns should not fail: %v", oc.Failures)
	}
}

func TestScoreSet_MissingResultFails(t *testing.T) {
	cases := []GoldenCase{
		{ID: "a", Expect: Expectation{TestsPass: true}},
		{ID: "b", Expect: Expectation{TestsPass: true}},
	}
	results := []CaseResult{{CaseID: "a", TestsPass: true}} // b has no result
	rep := ScoreSet(cases, results)
	if rep.Total != 2 || rep.Passed != 1 {
		t.Fatalf("Total/Passed = %d/%d, want 2/1", rep.Total, rep.Passed)
	}
	if rep.Score != 0.5 {
		t.Errorf("Score = %v, want 0.5", rep.Score)
	}
}

func TestDiffBaseline(t *testing.T) {
	prev := GoldenReport{Total: 3, Passed: 2, Score: 2.0 / 3.0, Cases: []CaseOutcome{
		{CaseID: "a", Passed: true},
		{CaseID: "b", Passed: true},
		{CaseID: "c", Passed: false},
	}}
	cur := GoldenReport{Total: 3, Passed: 2, Score: 2.0 / 3.0, Cases: []CaseOutcome{
		{CaseID: "a", Passed: true},
		{CaseID: "b", Passed: false}, // regression
		{CaseID: "c", Passed: true},  // fixed
	}}
	d := DiffBaseline(prev, cur)
	if len(d.Regressions) != 1 || d.Regressions[0] != "b" {
		t.Errorf("Regressions = %v, want [b]", d.Regressions)
	}
	if len(d.Fixed) != 1 || d.Fixed[0] != "c" {
		t.Errorf("Fixed = %v, want [c]", d.Fixed)
	}
}

func TestDiffBaseline_Removed(t *testing.T) {
	prev := GoldenReport{Cases: []CaseOutcome{{CaseID: "a", Passed: true}, {CaseID: "b", Passed: true}}}
	cur := GoldenReport{Cases: []CaseOutcome{{CaseID: "a", Passed: true}}}
	d := DiffBaseline(prev, cur)
	if len(d.Removed) != 1 || d.Removed[0] != "b" {
		t.Errorf("Removed = %v, want [b]", d.Removed)
	}
}

func TestValidateGoldenSet(t *testing.T) {
	if err := ValidateGoldenSet([]GoldenCase{{ID: "a"}, {ID: "b"}}); err != nil {
		t.Errorf("valid set errored: %v", err)
	}
	if err := ValidateGoldenSet([]GoldenCase{{ID: "a"}, {ID: "a"}}); err == nil {
		t.Error("duplicate id should error")
	}
	if err := ValidateGoldenSet([]GoldenCase{{ID: ""}}); err == nil {
		t.Error("empty id should error")
	}
}

func TestLoadGoldenSet_Example(t *testing.T) {
	cases, err := LoadGoldenSet(filepath.Join("testdata", "goldenset.example.json"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("example golden set is empty")
	}
	for _, c := range cases {
		if c.ID == "" || c.Prompt == "" {
			t.Errorf("golden case missing id/prompt: %+v", c)
		}
	}
}

func TestSaveLoadReportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	rep := GoldenReport{Total: 2, Passed: 1, Score: 0.5, Cases: []CaseOutcome{
		{CaseID: "a", Passed: true},
		{CaseID: "b", Passed: false, Failures: []string{"tests did not pass"}},
	}}
	if err := SaveGoldenReport(path, rep); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadGoldenReport(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Total != rep.Total || got.Passed != rep.Passed || len(got.Cases) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
