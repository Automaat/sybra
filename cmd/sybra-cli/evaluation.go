package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/prompteval"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func cmdEvaluation(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: evaluation <scan|judge|golden|offline> [args] [--json]")
	}
	switch args[0] {
	case "scan":
		return cmdEvaluationScan(cfg, jsonOut)
	case "judge":
		return cmdEvaluationJudge(store, args[1:], jsonOut)
	case "golden":
		return cmdEvaluationGolden(args[1:], jsonOut)
	case "offline":
		return cmdEvaluationOffline(cfg, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown evaluation subcommand: %s", args[0])
	}
}

// newOfflineRunner is overridable in tests so they can inject a fake runner
// instead of shelling out to a live provider CLI.
var newOfflineRunner = prompteval.SelectRunner

// offlineGoldenCase is the on-disk shape of one entry in --golden: a case
// input plus the assertions a variant's output must satisfy.
type offlineGoldenCase struct {
	CaseID     string                 `json:"caseId"`
	Input      string                 `json:"input"`
	Assertions []prompteval.Assertion `json:"assertions"`
}

func cmdEvaluationOffline(cfg *config.Config, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: evaluation offline <run|gate> [args]")
	}
	switch args[0] {
	case "run":
		return cmdEvaluationOfflineRun(cfg, args[1:], jsonOut)
	case "gate":
		return cmdEvaluationOfflineGate(cfg, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown evaluation offline subcommand: %s", args[0])
	}
}

// cmdEvaluationOfflineRun evaluates every candidate variant in --variants
// against every golden case in --golden, writes a VariantVerdict per variant,
// and exits non-zero if any variant scores FAIL (AC1: failed offline evals
// must be visible to CI, not swallowed). An empty/missing --variants list is
// a usage error, never a vacuous pass.
func cmdEvaluationOfflineRun(cfg *config.Config, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("offline run", flag.ContinueOnError)
	variantsPath := fs.String("variants", "", "path to a JSON array of candidate variants (required)")
	goldenPath := fs.String("golden", "", "path to a JSON array of golden cases (required)")
	outPath := fs.String("out", "", "optional path to write the run summary JSON")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *variantsPath == "" || *goldenPath == "" {
		return fatal(jsonOut, "usage: evaluation offline run --variants <v.json> --golden <g.json> [--out <path>]")
	}

	variants, err := loadOfflineVariants(*variantsPath)
	if err != nil {
		return fatal(jsonOut, "load variants: %v", err)
	}
	if len(variants) == 0 {
		return fatal(jsonOut, "--variants must contain at least one candidate variant")
	}
	if err := requireSameProviderModel(variants); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	cases, err := loadOfflineGoldenCases(*goldenPath)
	if err != nil {
		return fatal(jsonOut, "load golden cases: %v", err)
	}
	if len(cases) == 0 {
		return fatal(jsonOut, "--golden must contain at least one golden case")
	}

	runner := newOfflineRunner(cfg.Evaluation.Offline)
	store := prompteval.New(config.PromptEvalDir())

	unavailablePolicyPass := strings.EqualFold(strings.TrimSpace(cfg.Evaluation.Offline.UnavailablePolicy), "pass")

	ctx := context.Background()
	verdicts := make([]prompteval.VariantVerdict, 0, len(variants))
	anyFail := false
	for _, v := range variants {
		verdict := runOfflineVariant(ctx, runner, v, cases, cfg.Evaluation.Offline.MinScore)
		if err := store.Write(verdict); err != nil {
			return fatal(jsonOut, "write verdict for %s: %v", v.ID, err)
		}
		verdicts = append(verdicts, verdict)
		if verdict.Status == prompteval.StatusFail {
			anyFail = true
		}
		if verdict.Status == prompteval.StatusUnavailable && !unavailablePolicyPass {
			anyFail = true
		}
	}

	if *outPath != "" {
		data, mErr := json.MarshalIndent(verdicts, "", "  ")
		if mErr != nil {
			return fatal(jsonOut, "marshal summary: %v", mErr)
		}
		if wErr := os.WriteFile(*outPath, data, 0o644); wErr != nil {
			return fatal(jsonOut, "write summary: %v", wErr)
		}
	}

	if jsonOut {
		printJSON(map[string]any{"verdicts": verdicts})
	} else {
		for i := range verdicts {
			v := &verdicts[i]
			fmt.Printf("%s %s: %s (score %.2f)\n", v.VariantID, v.Digest[:min(12, len(v.Digest))], v.Status, v.Score)
			if v.Reason != "" {
				fmt.Printf("  %s\n", v.Reason)
			}
		}
	}
	if anyFail {
		return 1
	}
	return 0
}

// requireSameProviderModel enforces the offline runner's "run variants
// against the same model/provider settings" requirement: a batch is meant to
// screen prompt/skill variants against each other, and mixing
// provider/model settings within one run would make the comparison
// meaningless (or silently confound a prompt regression with a
// model/provider swap).
func requireSameProviderModel(variants []prompteval.CandidateVariant) error {
	provider, model := variants[0].Provider, variants[0].Model
	for _, v := range variants[1:] {
		if v.Provider != provider || v.Model != model {
			return fmt.Errorf("--variants must share the same provider/model: %s uses %s:%s, %s uses %s:%s",
				variants[0].ID, provider, model, v.ID, v.Provider, v.Model)
		}
	}
	return nil
}

// runOfflineVariant runs every golden case for one variant and aggregates
// into a single VariantVerdict. A runner error on any case (could not
// measure the run at all) marks the whole variant unavailable rather than a
// silent pass; otherwise the verdict is pass/fail against cfg.MinScore.
func runOfflineVariant(ctx context.Context, runner prompteval.OfflineRunner, v prompteval.CandidateVariant, cases []offlineGoldenCase, minScore float64) prompteval.VariantVerdict {
	digest := prompteval.Digest([]byte(v.Prompt))
	base := prompteval.VariantVerdict{
		VariantID: v.ID,
		Digest:    digest,
		Runner:    runner.Name(),
		CreatedAt: time.Now().UTC(),
	}
	if minScore <= 0 {
		minScore = 1.0
	}

	var totalScore, totalCost float64
	var totalLatency int64
	var assertions []prompteval.AssertionResult
	failedCase := ""
	for _, c := range cases {
		spec := prompteval.Spec{
			CaseID:     c.CaseID,
			Variant:    v,
			Input:      c.Input,
			Assertions: c.Assertions,
		}
		res, err := runner.Run(ctx, spec)
		if err != nil {
			base.Status = prompteval.StatusUnavailable
			base.Reason = fmt.Sprintf("case %s: %v", c.CaseID, err)
			return base
		}
		totalScore += res.Score
		totalCost += res.CostUSD
		totalLatency += res.LatencyMS
		assertions = append(assertions, res.Assertions...)
		if !res.Passed && failedCase == "" {
			failedCase = c.CaseID
		}
	}

	base.Score = totalScore / float64(len(cases))
	base.CostUSD = totalCost
	base.LatencyMS = totalLatency
	base.Assertions = assertions
	switch {
	case failedCase != "":
		// The runner's own pass/fail signal (e.g. promptfoo's
		// success/gradingResult.pass) is authoritative over Score — a runner
		// can report a high average score while still failing a hard
		// assertion on one case, and that must never aggregate to PASS.
		base.Status = prompteval.StatusFail
		base.Reason = fmt.Sprintf("case %s: runner reported failure", failedCase)
	case base.Score >= minScore:
		base.Status = prompteval.StatusPass
	default:
		base.Status = prompteval.StatusFail
		base.Reason = fmt.Sprintf("score %.2f below min %.2f", base.Score, minScore)
	}
	return base
}

// cmdEvaluationOfflineGate reads the stored verdict for (variantID, digest)
// and exits non-zero when AllowEnrollment blocks it (AC2 enforcement point).
func cmdEvaluationOfflineGate(cfg *config.Config, args []string, jsonOut bool) int {
	if len(args) != 2 {
		return fatal(jsonOut, "usage: evaluation offline gate <variant-id> <digest>")
	}
	store := prompteval.New(config.PromptEvalDir())
	gate := prompteval.NewGate(store, cfg.Evaluation.Offline)
	allow, reason, err := gate.AllowEnrollment(args[0], args[1])
	if err != nil {
		return fatal(jsonOut, "gate: %v", err)
	}
	switch {
	case jsonOut:
		printJSON(map[string]any{"allow": allow, "reason": reason})
	case allow:
		fmt.Println("allow")
	default:
		fmt.Printf("deny: %s\n", reason)
	}
	if !allow {
		return 1
	}
	return 0
}

func loadOfflineVariants(path string) ([]prompteval.CandidateVariant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var variants []prompteval.CandidateVariant
	if err := json.Unmarshal(data, &variants); err != nil {
		return nil, fmt.Errorf("parse variants: %w", err)
	}
	return variants, nil
}

func loadOfflineGoldenCases(path string) ([]offlineGoldenCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []offlineGoldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("parse golden cases: %w", err)
	}
	return cases, nil
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
		Cfg:       cfg.Evaluation,
		ABTesting: cfg.ABTesting,
		Stats:     statsStore,
		Audit:     evaluation.AuditDirReader(cfg.AuditDir()),
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
	fmt.Printf("  autonomy=%.0f%%  ci-first-pass=%.0f%%  failure=%.0f%% (%d/%d resolved runs)  change-failure=%.0f%% (%d reverted)  rework_tasks=%d\n",
		o.AutonomyRate*100, o.CIFirstPassRate*100, o.FailureRate*100, o.AgentFailures, o.AgentResolvedRuns,
		o.ChangeFailureRate*100, o.Reverted, o.ReworkTasks)
	fmt.Printf("  runs=%d  stalled=%d (retried, excluded from failure rate)\n", o.AgentRuns, o.AgentStalls)
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
