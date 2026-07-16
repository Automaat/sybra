package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func cmdPromptLab(cfg *config.Config, store *task.Manager, projStore *project.Store, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: prompt-lab <run> [flags]")
	}
	switch args[0] {
	case "run":
		return cmdPromptLabRun(cfg, store, projStore, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown prompt-lab subcommand: %s", args[0])
	}
}

func cmdPromptLabRun(cfg *config.Config, store *task.Manager, projStore *project.Store, args []string, jsonOut bool) int {
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
		filed, err = filePromptLabProposals(store, projStore, result)
		if err != nil {
			return fatal(jsonOut, "file proposals: %v", err)
		}
	}
	if jsonOut {
		return printJSON(map[string]any{"result": result, "filed": filed})
	}
	printPromptLabResult(result, filed)
	return 0
}

const promptLabProjectID = "Automaat/sybra"

// filePromptLabProposals files each proposal as a reviewed local task, skipping
// rejected/duplicate proposals and scrubbing the body before persistence. Scrub
// is EXPLICIT here, not inherited: fileHarnessProposals (harness_evolution.go)
// does not scrub, so this filer builds and applies its own work-typed
// blocklist per subject rather than assuming that precedent covers it too.
func filePromptLabProposals(store *task.Manager, projStore *project.Store, result promptlab.RunResult) ([]task.Task, error) {
	existing, err := store.List()
	if err != nil {
		return nil, err
	}
	var filed []task.Task
	for i := range result.Proposals {
		p := result.Proposals[i]
		if promptlab.HasProposal(existing, p.ID) {
			continue
		}
		body := scrubProposalBody(projStore, promptlab.RenderProposalBody(p), p.Evidence.ProjectIDs)
		tags := []string{promptlab.ProposalTag, "role:" + p.Subject.Role}
		status := task.StatusTodo
		if p.RequiresHumanApproval {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		update := task.Update{
			Status: &status,
			Tags:   &tags,
		}
		if projectID := promptLabTargetProjectID(projStore); projectID != "" {
			update.ProjectID = &projectID
		}
		created, err := store.CreateFull(p.Title, body, task.AgentModeHeadless, update)
		if err != nil {
			return filed, err
		}
		filed = append(filed, created)
		existing = append(existing, created)
	}
	return filed, nil
}

func promptLabTargetProjectID(projStore *project.Store) string {
	if projStore == nil {
		return ""
	}
	if _, err := projStore.Get(promptLabProjectID); err == nil {
		return promptLabProjectID
	}
	return ""
}

// scrubProposalBody redacts a proposal body against every work-typed
// project's blocklist among projectIDs. A project ID that fails to resolve
// or is pet-typed contributes nothing — fail-open on unknown/non-work
// projects mirrors App.workScrubContextForTask.
func scrubProposalBody(projStore *project.Store, body string, projectIDs []string) string {
	for _, id := range projectIDs {
		p, err := projStore.Get(id)
		if err != nil || p.Type != project.ProjectTypeWork {
			continue
		}
		blocklist := []string{p.ID, p.Owner, p.Repo}
		if p.URL != "" {
			blocklist = append(blocklist, p.URL)
		}
		body, _ = scrub.Scrub(body, blocklist)
	}
	return body
}

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
