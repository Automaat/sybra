package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
)

func cmdStats(cfg *config.Config, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: stats <lifecycle> [args] [--json]")
	}
	switch args[0] {
	case "lifecycle":
		return cmdStatsLifecycle(cfg, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown stats subcommand: %s", args[0])
	}
}

// cmdStatsLifecycle decomposes the lead time of tasks that landed in the window
// into per-phase durations (planning/implementing/testing/review/waiting), so
// you can see where end-to-end time actually goes.
func cmdStatsLifecycle(cfg *config.Config, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("lifecycle", flag.ContinueOnError)
	since := fs.String("since", "30d", "cohort window for landed tasks (duration like 7d/720h or date YYYY-MM-DD)")
	slowest := fs.Int("slowest", 10, "number of slowest landed tasks to list (0 = none)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	now := time.Now().UTC()
	sinceTime := parseSince(*since, now)
	// Read status history wider than the cohort window so a task that landed
	// in-window but started earlier is fully reconstructed.
	histSince := sinceTime.Add(-2 * now.Sub(sinceTime))
	evts, err := audit.Read(cfg.AuditDir(), audit.Query{Since: histSince, Until: now})
	if err != nil {
		return fatal(jsonOut, "read audit: %v", err)
	}

	rep := evaluation.ComputePhaseDurations(evts, sinceTime, now, *slowest)
	if jsonOut {
		return printJSON(rep)
	}
	printPhaseReport(rep)
	return 0
}

func printPhaseReport(rep evaluation.PhaseReport) {
	days := rep.Until.Sub(rep.Since).Hours() / 24
	fmt.Printf("lifecycle phases — %d landed tasks, last %.0fd\n", rep.Cohort, days)
	if rep.Cohort == 0 {
		fmt.Println("  (no tasks landed in the window — nothing to decompose)")
		return
	}

	// Share is share-of-lead-time across the cohort, so the denominator is the
	// summed time in each phase (TotalH) — not MeanH, which averages only over
	// tasks that entered the phase and would over-weight phases many tasks skip.
	var totalSum float64
	for _, p := range rep.Phases {
		totalSum += p.TotalH
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PHASE\tP50H\tP90H\tMEAN\tSHARE\tN")
	for _, p := range rep.Phases {
		share := 0.0
		if totalSum > 0 {
			share = p.TotalH / totalSum * 100
		}
		_, _ = fmt.Fprintf(w, "%s\t%.1f\t%.1f\t%.1f\t%.0f%%\t%d\n",
			p.Phase, p.P50H, p.P90H, p.MeanH, share, p.Count)
	}
	_ = w.Flush()

	if len(rep.Slowest) == 0 {
		return
	}
	fmt.Println("\nslowest landed tasks:")
	for _, t := range rep.Slowest {
		fmt.Printf("  %-12s %5.1fh  %s\n", t.TaskID, t.TotalH, formatByPhase(t.ByPhase))
	}
}

// formatByPhase renders a task's per-phase split in canonical order, e.g.
// "planning 6.0, implementing 9.0, review 3.4".
func formatByPhase(byPhase map[string]float64) string {
	keys := make([]string, 0, len(byPhase))
	for k := range byPhase {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return phaseRank(keys[i]) < phaseRank(keys[j])
	})
	out := ""
	for _, k := range keys {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s %.1f", k, byPhase[k])
	}
	return out
}

func phaseRank(phase string) int {
	order := []string{
		evaluation.PhaseQueued, evaluation.PhasePlanning, evaluation.PhaseImplementing,
		evaluation.PhaseTesting, evaluation.PhaseReview, evaluation.PhaseWaiting, evaluation.PhaseOther,
	}
	for i, p := range order {
		if p == phase {
			return i
		}
	}
	return len(order)
}
