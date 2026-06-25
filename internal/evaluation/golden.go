package evaluation

import (
	"encoding/json"
	"fmt"
	"os"
)

// GoldenCase is one representative task with known-good expectations. Running the
// fleet against a curated set of these catches regressions when prompts, skills,
// or models change — SWE-bench-style, scoped to Sybra's own workload.
type GoldenCase struct {
	ID        string      `json:"id"`
	Title     string      `json:"title"`
	ProjectID string      `json:"projectId,omitempty"`
	Prompt    string      `json:"prompt"`
	Expect    Expectation `json:"expect"`
}

// Expectation is the bar a golden case must clear. A zero field is unchecked.
type Expectation struct {
	TestsPass  bool    `json:"testsPass"`            // the run must end with tests passing
	MinQuality float64 `json:"minQuality,omitempty"` // minimum judge overall (0–10)
	MaxTurns   int     `json:"maxTurns,omitempty"`   // turn budget
}

// CaseResult is the observed outcome of executing a golden case. It is produced
// by the runner (live agent execution — out of scope for this engine) and fed
// to the scorer.
type CaseResult struct {
	CaseID    string  `json:"caseId"`
	TestsPass bool    `json:"testsPass"`
	Quality   float64 `json:"quality"`
	Turns     int     `json:"turns"`
}

// CaseOutcome is the scored result of one golden case.
type CaseOutcome struct {
	CaseID   string   `json:"caseId"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
}

// GoldenReport is the aggregate score of a golden-set run.
type GoldenReport struct {
	Total  int           `json:"total"`
	Passed int           `json:"passed"`
	Score  float64       `json:"score"` // passed / total
	Cases  []CaseOutcome `json:"cases"`
}

// GoldenDelta compares a fresh report against a baseline.
type GoldenDelta struct {
	ScoreDelta  float64  `json:"scoreDelta"`
	Regressions []string `json:"regressions,omitempty"` // passed in baseline, fail now
	Fixed       []string `json:"fixed,omitempty"`       // failed in baseline, pass now
	Removed     []string `json:"removed,omitempty"`     // in baseline, absent from this run
}

// ValidateGoldenSet rejects an empty set or one with empty/duplicate case IDs,
// which would make the score meaningless or let one result score two cases.
func ValidateGoldenSet(cases []GoldenCase) error {
	if len(cases) == 0 {
		return fmt.Errorf("golden set is empty")
	}
	seen := make(map[string]bool, len(cases))
	for i := range cases {
		id := cases[i].ID
		if id == "" {
			return fmt.Errorf("golden case %d has an empty id", i)
		}
		if seen[id] {
			return fmt.Errorf("duplicate golden case id %q", id)
		}
		seen[id] = true
	}
	return nil
}

// ScoreCase checks one observed result against its case's expectations.
func ScoreCase(c GoldenCase, r CaseResult) CaseOutcome {
	var fails []string
	if c.Expect.TestsPass && !r.TestsPass {
		fails = append(fails, "tests did not pass")
	}
	if c.Expect.MinQuality > 0 && r.Quality < c.Expect.MinQuality {
		fails = append(fails, fmt.Sprintf("quality %.1f < min %.1f", r.Quality, c.Expect.MinQuality))
	}
	if c.Expect.MaxTurns > 0 && r.Turns > c.Expect.MaxTurns {
		fails = append(fails, fmt.Sprintf("turns %d > max %d", r.Turns, c.Expect.MaxTurns))
	}
	return CaseOutcome{CaseID: c.ID, Passed: len(fails) == 0, Failures: fails}
}

// ScoreSet scores every case against its result. A case with no matching result
// fails explicitly, so a runner that silently skips a case can't inflate the score.
func ScoreSet(cases []GoldenCase, results []CaseResult) GoldenReport {
	byID := make(map[string]CaseResult, len(results))
	dup := make(map[string]bool)
	for _, r := range results {
		if _, seen := byID[r.CaseID]; seen {
			dup[r.CaseID] = true
		}
		byID[r.CaseID] = r
	}
	rep := GoldenReport{Total: len(cases)}
	for i := range cases {
		c := cases[i]
		// Duplicate results for one case mask a runner bug; fail it explicitly
		// rather than silently letting the last result win.
		if dup[c.ID] {
			rep.Cases = append(rep.Cases, CaseOutcome{CaseID: c.ID, Passed: false, Failures: []string{"duplicate results for case"}})
			continue
		}
		r, ok := byID[c.ID]
		if !ok {
			rep.Cases = append(rep.Cases, CaseOutcome{CaseID: c.ID, Passed: false, Failures: []string{"no result for case"}})
			continue
		}
		oc := ScoreCase(c, r)
		if oc.Passed {
			rep.Passed++
		}
		rep.Cases = append(rep.Cases, oc)
	}
	if rep.Total > 0 {
		rep.Score = float64(rep.Passed) / float64(rep.Total)
	}
	return rep
}

// DiffBaseline reports the score change and which cases regressed (passed in the
// baseline, fail now) or were fixed. Regressions are what gate a change.
func DiffBaseline(prev, cur GoldenReport) GoldenDelta {
	prevPass := make(map[string]bool, len(prev.Cases))
	for _, c := range prev.Cases {
		prevPass[c.CaseID] = c.Passed
	}
	d := GoldenDelta{ScoreDelta: cur.Score - prev.Score}
	curIDs := make(map[string]bool, len(cur.Cases))
	for _, c := range cur.Cases {
		curIDs[c.CaseID] = true
		was, existed := prevPass[c.CaseID]
		switch {
		case existed && was && !c.Passed:
			d.Regressions = append(d.Regressions, c.CaseID)
		case existed && !was && c.Passed:
			d.Fixed = append(d.Fixed, c.CaseID)
		}
	}
	// Cases dropped from the set since the baseline — coverage loss worth surfacing.
	for _, c := range prev.Cases {
		if !curIDs[c.CaseID] {
			d.Removed = append(d.Removed, c.CaseID)
		}
	}
	return d
}

// LoadGoldenSet reads a JSON array of golden cases.
func LoadGoldenSet(path string) ([]GoldenCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []GoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("parse golden set: %w", err)
	}
	return cases, nil
}

// LoadCaseResults reads a JSON array of observed case results.
func LoadCaseResults(path string) ([]CaseResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results []CaseResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("parse results: %w", err)
	}
	return results, nil
}

// LoadGoldenReport reads a persisted report (used as a baseline).
func LoadGoldenReport(path string) (GoldenReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GoldenReport{}, err
	}
	var rep GoldenReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return GoldenReport{}, fmt.Errorf("parse report: %w", err)
	}
	return rep, nil
}

// SaveGoldenReport writes a report as indented JSON (the new baseline).
func SaveGoldenReport(path string, rep GoldenReport) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
