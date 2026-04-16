package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
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
	store *task.Manager,
	projStore *project.Store,
	args []string,
	jsonOut bool,
) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: triage <classify> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "classify":
		return cmdTriageClassify(cfg, store, projStore, rest, jsonOut)
	default:
		return fatal(jsonOut, "unknown triage command: %s", sub)
	}
}

func cmdTriageClassify(
	cfg *config.Config,
	store *task.Manager,
	projStore *project.Store,
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

	projects, err := projStore.List()
	if err != nil {
		return fatal(jsonOut, "list projects: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	classifier := &triage.ClaudeClassifier{Model: *model, Logger: logger}
	al, _ := audit.NewLogger(cfg.AuditDir())

	var targets []task.Task
	switch {
	case *all:
		list, listErr := store.List()
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
		t, getErr := store.Get(id)
		if getErr != nil {
			return fatal(jsonOut, "get %s: %v", id, getErr)
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
		result, classErr := classifyOne(classifier, store, al, targets[i], projects, *timeout)
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

	if jsonOut {
		switch {
		case *all:
			_ = printJSON(results)
		case len(results) == 1:
			_ = printJSON(results[0])
		default:
			_ = printJSON(results)
		}
	} else {
		for i := range results {
			fmt.Printf("Classified %s → %s (%s, %s, %s)\n",
				results[i].Task.ID,
				results[i].Verdict.Title,
				results[i].Verdict.Size,
				results[i].Verdict.Type,
				results[i].Task.Status,
			)
		}
	}

	if hadErr {
		return 1
	}
	return 0
}

func classifyOne(
	classifier triage.Classifier,
	store *task.Manager,
	al *audit.Logger,
	t task.Task,
	projects []project.Project,
	timeout time.Duration,
) (triageResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	v, err := classifier.Classify(ctx, t, projects)
	if err != nil {
		return triageResult{}, err
	}
	updated, err := triage.Apply(store, t, v, projects)
	if err != nil {
		return triageResult{}, err
	}
	if al != nil {
		_ = al.Log(audit.Event{
			Type:   audit.EventTriageClassified,
			TaskID: t.ID,
			Data: map[string]any{
				"title":      v.Title,
				"tags":       v.Tags,
				"size":       v.Size,
				"type":       v.Type,
				"mode":       v.Mode,
				"project_id": updated.ProjectID,
				"status":     string(updated.Status),
			},
		})
	}
	return triageResult{Verdict: v, Task: updated}, nil
}
