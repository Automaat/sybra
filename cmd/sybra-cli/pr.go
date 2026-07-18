package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// cmdPR routes the `pr` subcommands.
func cmdPR(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	if len(args) == 0 {
		return fatal(jsonOut, "usage: pr create <task-id> --repo owner/name --head branch [--title T] [--body B] [--dir D] [--draft]")
	}
	switch args[0] {
	case "create":
		return cmdPRCreate(s, api, args[1:], jsonOut)
	default:
		return fatal(jsonOut, "unknown pr subcommand %q (valid: create)", args[0])
	}
}

// getTaskViaAPIOrFS reads a task through the server when the API is reachable,
// falling back to the local store. Mirrors updateTaskViaAPIOrFS: a Job that has
// only the HTTP endpoint and no mounted tasks dir must still be able to read the
// task it is opening a PR for.
func getTaskViaAPIOrFS(s *task.Manager, api *apiClient, id string) (task.Task, error) {
	if got, handled, apiErr := viaAPI[task.Task](api, "TaskService", "GetTask", id); handled {
		return got, apiErr
	}
	return s.Get(id)
}

// cmdPRCreate opens a pull request for an already-pushed branch and records its
// number on the task, in one step.
//
// It exists so a Kubernetes agent Job can open its own PR. The server-side
// create_pr workflow step runs `gh` in the task worktree, which forces the
// server to hold a GitHub credential and to own repo state — the opposite of
// what a k8s deployment wants, where the server only dispatches Jobs and every
// repo operation happens in the Job that already has the clone and the token.
// The task update goes through the same API-or-filesystem path as link-pr, so a
// Job that only has the HTTP endpoint (no shared task dir) still reports back.
func cmdPRCreate(s *task.Manager, api *apiClient, args []string, jsonOut bool) int {
	// Pull the task id off before parsing, matching cmdUpdate: flag.Parse stops
	// at the first non-flag argument, so `pr create <id> --repo ...` would
	// otherwise silently ignore every flag after the id.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fatal(jsonOut, "usage: pr create <task-id> --repo owner/name --head branch [--title T] [--body B] [--dir D] [--draft]")
	}
	id := args[0]

	fs := flag.NewFlagSet("pr create", flag.ContinueOnError)
	repo := fs.String("repo", "", "base repo the PR targets, owner/name")
	head := fs.String("head", "", "branch to open the PR from; fork-owner:branch for a fork")
	title := fs.String("title", "", "PR title")
	body := fs.String("body", "", "PR body")
	dir := fs.String("dir", ".", "directory to run gh in; must be inside the repo clone")
	draft := fs.Bool("draft", false, "open the PR as a draft")
	if err := fs.Parse(args[1:]); err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if *repo == "" || *head == "" {
		return fatal(jsonOut, "--repo and --head are required")
	}

	t, err := getTaskViaAPIOrFS(s, api, id)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}
	if t.PRNumber > 0 {
		return fatal(jsonOut, "task %s already has PR #%d; refusing to open a second one", t.ID, t.PRNumber)
	}
	if *title == "" {
		*title = t.Title
	}

	num, _, err := github.CreatePR(context.Background(), *dir, github.CreatePRRequest{
		Repo:  *repo,
		Head:  *head,
		Title: *title,
		Body:  *body,
		Draft: *draft,
	})
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	// Record the number only — deliberately not link-pr's advance-to-in-review.
	// This runs from inside a workflow's run_agent step, and the workflow owns
	// status: simple-task-pr's maybe_create_pr guard already routes to
	// push_existing_pr once pr_number exists, then advances. Flipping status
	// here would jump the task past the step that is still running.
	updated, err := updateTaskViaAPIOrFS(s, api, id, map[string]any{"pr_number": float64(num)})
	if err != nil {
		// The PR is already open; failing silently here would strand it
		// unlinked and invisible to the monitor loop.
		return fatal(jsonOut, "opened PR #%d but could not record it on task %s: %v", num, id, err)
	}

	if jsonOut {
		return printJSON(updated)
	}
	fmt.Printf("opened PR #%d for task %s\n", num, updated.ID)
	return 0
}
