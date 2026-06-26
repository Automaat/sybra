package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func cmdEvaluation(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: evaluation <scan|judge|golden> [args] [--json]")
	}
	switch args[0] {
	case "scan":
		return cmdEvaluationScan(cfg, jsonOut)
	case "judge":
		return cmdEvaluationJudge(store, args[1:], jsonOut)
	case "golden":
		return cmdEvaluationGolden(args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown evaluation subcommand: %s", args[0])
	}
}

// cmdEvaluationGolden scores a golden-set run's results against expectations and,
// when a baseline is given, reports regressions. Exits non-zero on any failing
// case or regression so it can gate prompt/skill/model changes in CI.
func cmdEvaluationGolden(args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("golden", flag.ContinueOnError)
	setPath := fs.String("set", "", "path to the golden-set JSON (required)")
	resultsPath := fs.String("results", "", "path to the case-results JSON (required)")
	baselinePath := fs.String("baseline", "", "optional baseline report to diff against")
	updateBaseline := fs.Bool("update-baseline", false, "write the fresh report to --baseline")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *setPath == "" || *resultsPath == "" {
		return fatal(jsonOut, "usage: evaluation golden --set <set.json> --results <results.json> [--baseline <b.json>] [--update-baseline]")
	}
	if *updateBaseline && *baselinePath == "" {
		return fatal(jsonOut, "--update-baseline requires --baseline")
	}
	cases, err := evaluation.LoadGoldenSet(*setPath)
	if err != nil {
		return fatal(jsonOut, "load set: %v", err)
	}
	if err := evaluation.ValidateGoldenSet(cases); err != nil {
		return fatal(jsonOut, "invalid golden set: %v", err)
	}
	results, err := evaluation.LoadCaseResults(*resultsPath)
	if err != nil {
		return fatal(jsonOut, "load results: %v", err)
	}
	rep := evaluation.ScoreSet(cases, results)

	var delta *evaluation.GoldenDelta
	if *baselinePath != "" {
		if prev, err := evaluation.LoadGoldenReport(*baselinePath); err == nil {
			d := evaluation.DiffBaseline(prev, rep)
			delta = &d
		} else if !os.IsNotExist(err) {
			return fatal(jsonOut, "load baseline: %v", err)
		}
	}

	// Gate: any failing case is a hard floor (protects even with no baseline),
	// and any regression vs the baseline. A clean run is required to bless a new
	// baseline, so --update-baseline can never record a failing/regressed report.
	clean := rep.Passed == rep.Total && (delta == nil || len(delta.Regressions) == 0)
	if *updateBaseline {
		if !clean {
			return fatal(jsonOut, "refusing to update baseline from a failing/regressed run")
		}
		if err := evaluation.SaveGoldenReport(*baselinePath, rep); err != nil {
			return fatal(jsonOut, "write baseline: %v", err)
		}
	}

	if jsonOut {
		printJSON(map[string]any{"report": rep, "delta": delta})
	} else {
		fmt.Printf("golden: %d/%d passed (score %.2f)\n", rep.Passed, rep.Total, rep.Score)
		for _, c := range rep.Cases {
			if !c.Passed {
				fmt.Printf("  FAIL %s: %s\n", c.CaseID, strings.Join(c.Failures, "; "))
			}
		}
		if delta != nil {
			fmt.Printf("  vs baseline: score %+.2f", delta.ScoreDelta)
			if len(delta.Fixed) > 0 {
				fmt.Printf("  fixed=%s", strings.Join(delta.Fixed, ","))
			}
			if len(delta.Regressions) > 0 {
				fmt.Printf("  REGRESSIONS=%s", strings.Join(delta.Regressions, ","))
			}
			if len(delta.Removed) > 0 {
				fmt.Printf("  removed=%s", strings.Join(delta.Removed, ","))
			}
			fmt.Println()
		}
	}
	if !clean {
		return 1 // gate: failing cases or a regression
	}
	return 0
}

func cmdEvaluationScan(cfg *config.Config, jsonOut bool) int {
	statsStore, err := stats.NewStore(config.StatsFile())
	if err != nil {
		return fatal(jsonOut, "open stats: %v", err)
	}
	svc := evaluation.NewService(evaluation.Deps{
		Cfg:   cfg.Evaluation,
		Stats: statsStore,
		Audit: evaluation.AuditDirReader(cfg.AuditDir()),
	})
	report, err := svc.Scan(context.Background())
	if err != nil {
		return fatal(jsonOut, "scan: %v", err)
	}
	if jsonOut {
		return printJSON(report)
	}
	o := report.Overall
	fmt.Printf("evaluation (%dd window): landed=%d merged=%d merged_with_edits=%d closed=%d\n",
		int(o.WindowDays), o.TasksLanded, o.Merged, o.MergedWithEdits, o.Closed)
	fmt.Printf("  autonomy=%.0f%%  ci-first-pass=%.0f%%  failure=%.0f%%  change-failure=%.0f%% (%d reverted)  rework_tasks=%d\n",
		o.AutonomyRate*100, o.CIFirstPassRate*100, o.FailureRate*100, o.ChangeFailureRate*100, o.Reverted, o.ReworkTasks)
	fmt.Printf("  lead p50/p90=%.1f/%.1fh  cycle p50/p90=%.1f/%.1fh\n",
		o.LeadTimeP50H, o.LeadTimeP90H, o.CycleTimeP50H, o.CycleTimeP90H)
	fmt.Printf("  cost=$%.2f ($%.2f/landed)  turns/landed=%.1f  tools/landed=%.1f\n",
		o.TotalCostUSD, o.CostPerLanded, o.TurnsPerLanded, o.ToolsPerLanded)
	if len(report.Weaknesses) > 0 {
		fmt.Println("weaknesses:")
		for _, w := range report.Weaknesses {
			fmt.Printf("  [%s] %s: %s\n    → %s\n", w.Severity, w.Metric, w.Detail, w.Suggestion)
		}
	}
	return 0
}

func cmdEvaluationJudge(store *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("judge", flag.ContinueOnError)
	model := fs.String("model", "", "judge model (default claude-sonnet-4-6)")
	seed := fs.Int64("seed", 0, "rubric shuffle seed (0 = stable order)")
	timeout := fs.Duration("timeout", 3*time.Minute, "max time to wait for the judge")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fatal(jsonOut, "usage: evaluation judge <task-id> [--model M] [--seed N]")
	}
	t, err := store.Get(rest[0])
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fatal(jsonOut, "task %s has no PR/project to judge", t.ID)
	}
	diff, err := github.FetchPRDiff(t.ProjectID, t.PRNumber)
	if err != nil {
		return fatal(jsonOut, "fetch diff: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	judge := &evaluation.ClaudeQualityJudge{Model: *model, DimSeed: *seed}
	v, err := judge.Judge(ctx, evaluation.JudgeRequest{
		TaskID:     t.ID,
		Title:      t.Title,
		Body:       t.Body,
		Diff:       diff,
		Trajectory: trajectorySummary(t),
	})
	if err != nil {
		return fatal(jsonOut, "judge: %v", err)
	}
	if jsonOut {
		return printJSON(v)
	}
	fmt.Printf("quality verdict for %s (PR #%d): overall %.1f/10\n", t.ID, t.PRNumber, v.Overall)
	for _, d := range evaluation.Rubric {
		s := v.Dimensions[d.Key]
		fmt.Printf("  %-18s %2d/10  %s\n", d.Key, s.Score, s.Rationale)
	}
	if v.Summary != "" {
		fmt.Printf("  summary: %s\n", v.Summary)
	}
	if t.Outcome != "" {
		fmt.Printf("  outcome=%s  judge-agrees=%v\n", t.Outcome, evaluation.AgreesWithOutcome(v, t.Outcome, 6.0))
	}
	return 0
}

// trajectorySummary renders the task's agent-run history into a one-line summary
// the judge can use to assess how the agent reached the result (agent-as-judge).
func trajectorySummary(t task.Task) string {
	if len(t.AgentRuns) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.AgentRuns))
	for i := range t.AgentRuns {
		r := t.AgentRuns[i]
		role := r.Role
		if role == "" {
			role = "implementation"
		}
		parts = append(parts, fmt.Sprintf("%s(%s $%.2f)", role, r.State, r.CostUSD))
	}
	return fmt.Sprintf("%d agent runs: %s", len(t.AgentRuns), strings.Join(parts, " → "))
}
