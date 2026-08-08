package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/harnessevolution"
	"github.com/Automaat/sybra/internal/task"
)

func cmdHarnessEvolution(cfg *config.Config, api *apiClient, store taskBoard, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: harness-evolution <run> [flags]")
	}
	switch args[0] {
	case "run":
		return cmdHarnessEvolutionRun(cfg, api, store, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown harness-evolution subcommand: %s", args[0])
	}
}

func cmdHarnessEvolutionRun(cfg *config.Config, api *apiClient, store taskBoard, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("harness-evolution run", flag.ContinueOnError)
	defaultLookback := time.Duration(cfg.HarnessEvolve.LookbackHours * float64(time.Hour))
	lookbackFlag := fs.String("lookback", defaultLookback.String(), "lookback window (e.g. 168h or 7d)")
	minCluster := fs.Int("min-cluster-size", cfg.HarnessEvolve.MinClusterSize, "minimum events required to propose")
	corpusDir := fs.String("corpus", "", "optional directory of regression corpus JSON files")
	fileTasks := fs.Bool("file", false, "create local Sybra proposal tasks")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	lookback, err := parseDurationFlag(*lookbackFlag)
	if err != nil {
		return fatal(jsonOut, "parse --lookback: %v", err)
	}
	// Mining reads the instance's persisted self-monitor report and filing
	// writes its board, so a reachable server runs both halves; splitting
	// them would mine one machine's report and file on another's board.
	res, err := callAPIWithin[harnessEvolutionRunDTO](api, apiSlowCallTimeout, selfMonitorServiceName,
		"RunHarnessEvolution", int64(lookback/time.Second), *minCluster, *corpusDir, *fileTasks)
	if err != nil {
		return fatal(jsonOut, "harness evolution: %v", err)
	}
	return reportHarnessEvolution(jsonOut, res.Result, res.Filed)
}

// harnessEvolutionRunDTO mirrors the server's wire shape for a mining run.
type harnessEvolutionRunDTO struct {
	Result harnessevolution.RunResult `json:"result"`
	Filed  []task.Task                `json:"filed"`
}

func reportHarnessEvolution(jsonOut bool, result harnessevolution.RunResult, filed []task.Task) int {
	if jsonOut {
		return printJSON(map[string]any{"result": result, "filed": filed})
	}
	printHarnessEvolutionResult(result, filed)
	return 0
}

func printHarnessEvolutionResult(result harnessevolution.RunResult, filed []task.Task) {
	fmt.Printf("harness-evolution: state=%s events=%d clusters=%d proposals=%d filed=%d\n",
		result.State, result.Events, len(result.Clusters), len(result.Proposals), len(filed))
	if result.Reason != "" {
		fmt.Printf("  reason: %s\n", result.Reason)
	}
	if len(result.Proposals) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tKIND\tREC\tGATE\tTRACES\tTITLE")
	for i := range result.Proposals {
		p := result.Proposals[i]
		gate := "standard"
		if p.RequiresHumanApproval {
			gate = "human"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			p.ID, p.Kind, p.Evaluation.Recommendation, gate, len(p.Evidence), p.Title)
	}
	_ = w.Flush()
}
