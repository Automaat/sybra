package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func cmdPromptLab(cfg *config.Config, api *apiClient, store taskBoard, projStore projectBoard, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: prompt-lab <run> [flags]")
	}
	switch args[0] {
	case "run":
		return cmdPromptLabRun(cfg, api, store, projStore, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown prompt-lab subcommand: %s", args[0])
	}
}

func cmdPromptLabRun(cfg *config.Config, api *apiClient, store taskBoard, projStore projectBoard, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("prompt-lab run", flag.ContinueOnError)
	defaultLookback := time.Duration(cfg.PromptLab.LookbackHours * float64(time.Hour))
	lookbackFlag := fs.String("lookback", defaultLookback.String(), "lookback window (e.g. 168h or 7d)")
	minSamples := fs.Int("min-samples", cfg.PromptLab.MinSamples, "minimum runs required to propose")
	fileTasks := fs.Bool("file", false, "create local Sybra proposal tasks")
	dryRun := fs.Bool("dry-run", false, "compute proposals without filing tasks, even if --file is set")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	lookback, err := parseDurationFlag(*lookbackFlag)
	if err != nil {
		return fatal(jsonOut, "parse --lookback: %v", err)
	}

	// The run history the evidence comes from and the board the proposals
	// land on both belong to the owning instance, so a reachable server runs
	// the whole operation rather than mining one machine for another.
	if api != nil {
		res, err := callAPIWithin[promptLabRunDTO](api, apiSlowCallTimeout, promptLabServiceName,
			"RunPromptLab", int64(lookback/time.Second), *minSamples, *fileTasks && !*dryRun)
		if err != nil {
			return fatal(jsonOut, "prompt lab: %v", err)
		}
		return reportPromptLab(jsonOut, res.Result, res.Filed)
	}

	statsStore, err := stats.NewStore(config.StatsFile())
	if err != nil {
		return fatal(jsonOut, "open stats: %v", err)
	}

	result, err := promptlab.Run(context.Background(), promptlab.Options{
		Records:       statsStore.All(),
		OutputDir:     config.PromptLabDir(),
		Lookback:      lookback,
		MinSamples:    *minSamples,
		MinEffectSize: cfg.PromptLab.MinEffectSize,
		MaxProposals:  cfg.PromptLab.MaxProposalsPerRun,
	})
	if err != nil {
		return fatal(jsonOut, "prompt lab: %v", err)
	}

	var filed []task.Task
	if *fileTasks && !*dryRun {
		cooldown := time.Duration(cfg.PromptLab.RefileCooldownDays * float64(24*time.Hour))
		filed, err = promptlab.FileProposals(store, projStore, result, cooldown, time.Now().UTC())
		if err != nil {
			return fatal(jsonOut, "file proposals: %v", err)
		}
	}
	return reportPromptLab(jsonOut, result, filed)
}

// promptLabRunDTO mirrors the server's wire shape for a mining run.
type promptLabRunDTO struct {
	Result promptlab.RunResult `json:"result"`
	Filed  []task.Task         `json:"filed"`
}

func reportPromptLab(jsonOut bool, result promptlab.RunResult, filed []task.Task) int {
	if jsonOut {
		return printJSON(map[string]any{"result": result, "filed": filed})
	}
	printPromptLabResult(result, filed)
	return 0
}

// scrubProposalBody redacts a proposal body against every work-typed
// project's blocklist among projectIDs. A project ID that fails to resolve
// or is pet-typed contributes nothing — fail-open on unknown/non-work
// projects mirrors App.workScrubContextForTask.

func printPromptLabResult(result promptlab.RunResult, filed []task.Task) {
	fmt.Printf("prompt-lab: weak_subjects=%d proposals=%d dropped=%d filed=%d\n",
		len(result.WeakSubjects), len(result.Proposals), result.Dropped, len(filed))
	if len(result.Proposals) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tROLE\tINTENT\tVERDICT\tGATE\tTITLE")
	for i := range result.Proposals {
		p := result.Proposals[i]
		gate := "standard"
		if p.RequiresHumanApproval {
			gate = "human"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.ID, p.Subject.Role, p.Candidate.Intent, p.Offline.Verdict, gate, p.Title)
	}
	_ = w.Flush()
}
