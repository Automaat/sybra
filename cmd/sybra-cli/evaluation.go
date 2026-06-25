package main

import (
	"context"
	"fmt"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/stats"
)

func cmdEvaluation(cfg *config.Config, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: evaluation <scan> [--json]")
	}
	switch args[0] {
	case "scan":
		return cmdEvaluationScan(cfg, jsonOut)
	default:
		return fatal(jsonOut, "unknown evaluation subcommand: %s", args[0])
	}
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
	fmt.Printf("evaluation (%dd window): landed=%d merged=%d closed=%d\n",
		int(o.WindowDays), o.TasksLanded, o.Merged, o.Closed)
	fmt.Printf("  autonomy=%.0f%%  ci-first-pass=%.0f%%  failure=%.0f%%  rework_tasks=%d\n",
		o.AutonomyRate*100, o.CIFirstPassRate*100, o.FailureRate*100, o.ReworkTasks)
	fmt.Printf("  lead p50/p90=%.1f/%.1fh  cycle p50/p90=%.1f/%.1fh\n",
		o.LeadTimeP50H, o.LeadTimeP90H, o.CycleTimeP50H, o.CycleTimeP90H)
	fmt.Printf("  cost=$%.2f ($%.2f/landed)  turns/landed=%.1f  tools/landed=%.1f\n",
		o.TotalCostUSD, o.CostPerLanded, o.TurnsPerLanded, o.ToolsPerLanded)
	return 0
}
