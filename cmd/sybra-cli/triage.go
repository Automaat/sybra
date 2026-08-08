package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
)

// triageResult is the CLI's JSON output for a classify call.
type triageResult struct {
	Verdict triage.Verdict `json:"verdict"`
	Task    task.Task      `json:"task"`
}

func cmdTriage(
	cfg *config.Config,
	api *apiClient,
	board taskBoard,
	args []string,
	jsonOut bool,
) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: triage <classify> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "classify":
		return cmdTriageClassify(cfg, api, board, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown triage command: %s", sub)
	}
}

func cmdTriageClassify(
	cfg *config.Config,
	api *apiClient,
	board taskBoard,
	args []string,
	jsonOut bool,
) int {
	fs := flag.NewFlagSet("triage classify", flag.ContinueOnError)
	all := fs.Bool("all", false, "classify every task with status=new")
	model := fs.String("model", cfg.Triage.Model, "claude model")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-task LLM timeout")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "%v", err)
	}

	var targets []task.Task
	switch {
	case *all:
		list, listErr := board.List()
		if listErr != nil {
			return fatal(jsonOut, "list tasks: %v", listErr)
		}
		for i := range list {
			if list[i].Status == task.StatusNew {
				targets = append(targets, list[i])
			}
		}
	case len(fs.Args()) == 1:
		id := fs.Args()[0]
		t, getErr := board.Get(id)
		if getErr != nil {
			return fatal(jsonOut, "get %s: %v", id, getErr)
		}
		// Mirror the --all path (restricted to status=new above) and the
		// poll-based auto-triage handler (internal/poll/triage.go), which both
		// only ever classify fresh tasks. Without this guard, classifying an
		// arbitrary id can reclassify a task another subsystem owns outside the
		// triage pipeline — e.g. a human-required Prompt Lab proposal, whose
		// gating tags/status Apply must not touch (see internal/triage/apply.go).
		if t.Status != task.StatusNew {
			return fatal(jsonOut, "task %s has status %q, not %q — triage classify only reclassifies fresh tasks",
				id, t.Status, task.StatusNew)
		}
		targets = append(targets, t)
	default:
		return fatal(jsonOut, "usage: triage classify <id> | triage classify --all")
	}

	if len(targets) == 0 {
		if jsonOut {
			return printJSON([]triageResult{})
		}
		fmt.Println("no tasks to classify")
		return 0
	}

	results := make([]triageResult, 0, len(targets))
	var hadErr bool
	for i := range targets {
		result, classErr := classifyOne(api, targets[i], *timeout, *model)
		if classErr != nil {
			hadErr = true
			if jsonOut {
				fmt.Fprintf(os.Stderr, `{"error":"classify %s: %v"}`+"\n", targets[i].ID, classErr)
			} else {
				fmt.Fprintf(os.Stderr, "classify %s: %v\n", targets[i].ID, classErr)
			}
			continue
		}
		results = append(results, result)
	}

	reportTriageResults(jsonOut, *all, results)

	if hadErr {
		return 1
	}
	return 0
}

func reportTriageResults(jsonOut, all bool, results []triageResult) {
	if !jsonOut {
		for i := range results {
			fmt.Printf(
				"Classified %s → %s (%s, %s, %s)\n",
				results[i].Task.ID,
				results[i].Verdict.Title,
				results[i].Verdict.Size,
				results[i].Verdict.Type,
				results[i].Task.Status,
			)
		}
		return
	}
	if !all && len(results) == 1 {
		_ = printJSON(results[0])
		return
	}
	_ = printJSON(results)
}

// classifyOne applies its verdict atomically, so the server runs the whole
// operation and the apply lands under the locks it holds.
func classifyOne(api *apiClient, t task.Task, timeout time.Duration, model string) (triageResult, error) {
	return callAPIWithin[triageResult](api, max(timeout, apiCallTimeout), taskServiceName, "ClassifyTask", t.ID, model)
}

func markTriageRetryable(store *task.Manager, t task.Task, err error) error {
	reason := triage.RetryableStatusReason(err)
	_, updateErr := store.Update(t.ID, task.Update{StatusReason: &reason})
	return updateErr
}
