package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/harnessevolution"
	"github.com/Automaat/sybra/internal/task"
)

func cmdHarnessEvolution(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: harness-evolution <run> [flags]")
	}
	switch args[0] {
	case "run":
		return cmdHarnessEvolutionRun(cfg, store, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown harness-evolution subcommand: %s", args[0])
	}
}

func cmdHarnessEvolutionRun(cfg *config.Config, store *task.Manager, args []string, jsonOut bool) int {
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
	result, err := harnessevolution.Run(context.Background(), harnessevolution.Options{
		ReportPath:     config.SelfMonitorLastReportPath(),
		CorpusDir:      *corpusDir,
		OutputDir:      config.HarnessEvolveDir(),
		Lookback:       lookback,
		MinClusterSize: *minCluster,
	})
	if err != nil {
		return fatal(jsonOut, "harness evolution: %v", err)
	}
	var filed []task.Task
	if *fileTasks {
		filed, err = fileHarnessProposals(store, result)
		if err != nil {
			return fatal(jsonOut, "file proposals: %v", err)
		}
	}
	if jsonOut {
		return printJSON(map[string]any{"result": result, "filed": filed})
	}
	printHarnessEvolutionResult(result, filed)
	return 0
}

func fileHarnessProposals(store *task.Manager, result harnessevolution.RunResult) ([]task.Task, error) {
	var filed []task.Task
	existing, err := store.List()
	if err != nil {
		return nil, err
	}
	for i := range result.Proposals {
		p := result.Proposals[i]
		if p.Evaluation.Recommendation == harnessevolution.RecommendationReject {
			continue
		}
		if _, ok := findExistingHarnessProposal(existing, p.ID); ok {
			continue
		}
		cluster := result.Clusters[i]
		body := harnessevolution.RenderProposalBody(p, cluster)
		tags := []string{"harness-proposal", string(p.Kind), string(p.Evaluation.Recommendation)}
		status := task.StatusTodo
		if p.RequiresHumanApproval || p.Evaluation.Recommendation == harnessevolution.RecommendationNeedsHumanReview {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		created, err := store.CreateFull(p.Title, body, task.AgentModeHeadless, task.Update{
			Status: &status,
			Tags:   &tags,
		})
		if err != nil {
			return filed, err
		}
		filed = append(filed, created)
		existing = append(existing, created)
	}
	return filed, nil
}

func findExistingHarnessProposal(tasks []task.Task, proposalID string) (task.Task, bool) {
	marker := "Proposal ID:** `" + proposalID + "`"
	for i := range tasks {
		if task.IsTerminalStatus(tasks[i].Status) {
			continue
		}
		if !hasTag(tasks[i].Tags, "harness-proposal") {
			continue
		}
		if strings.Contains(tasks[i].Body, marker) {
			return tasks[i], true
		}
	}
	return task.Task{}, false
}

func hasTag(tags []string, want string) bool {
	return slices.Contains(tags, want)
}

func printHarnessEvolutionResult(result harnessevolution.RunResult, filed []task.Task) {
	fmt.Printf("harness-evolution: events=%d clusters=%d proposals=%d filed=%d\n",
		result.Events, len(result.Clusters), len(result.Proposals), len(filed))
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
