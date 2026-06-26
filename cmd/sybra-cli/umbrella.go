package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// cmdUmbrella expands a GitHub umbrella issue into a gated task DAG: one
// `umbrella` tracker task plus one `blocked` child per sub-issue, with
// dependency edges extracted by an LLM planner. Re-running is idempotent —
// only sub-issues without an existing task are materialized.
func cmdUmbrella(s *task.Manager, args []string, jsonOut bool) int {
	fs := flag.NewFlagSet("umbrella", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "umbrella issue URL (or pass as first argument)")
	model := fs.String("model", "", "planner model (default: claude default)")
	if err := fs.Parse(args); err != nil {
		return fatal(jsonOut, "parse flags: %v", err)
	}
	issueURL := *urlFlag
	if issueURL == "" && fs.NArg() > 0 {
		issueURL = fs.Arg(0)
	}
	if issueURL == "" {
		return fatal(jsonOut, "umbrella: an issue URL is required")
	}

	repo, number, err := parseIssueURL(issueURL)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	umb, subs, err := github.FetchUmbrella(repo, number)
	if err != nil {
		return fatal(jsonOut, "fetch umbrella: %v", err)
	}
	if len(subs) == 0 {
		return fatal(jsonOut, "umbrella %s has no sub-issues", issueURL)
	}

	// Index sub-issues by canonical (URL) ref for planner input + later lookup.
	planSubs := make([]umbrella.SubIssue, len(subs))
	byRef := make(map[string]github.Issue, len(subs))
	for i := range subs {
		planSubs[i] = umbrella.SubIssue{Ref: subs[i].URL, Title: subs[i].Title, Body: subs[i].Body}
		byRef[umbrella.NormalizeIssueRef(subs[i].URL)] = subs[i]
	}

	plan, err := umbrella.Generate(context.Background(), claudePlannerRunner(*model), umb.URL, umb.Body, planSubs)
	if err != nil {
		return fatal(jsonOut, "plan umbrella: %v", err)
	}

	existing, trackerExists := scanExisting(s, umb.URL)
	specs := umbrella.ChildSpecs(plan, planSubs, existing)

	created, err := materializeUmbrella(s, umb, repo, specs, byRef, trackerExists)
	if err != nil {
		return fatal(jsonOut, "%v", err)
	}

	return reportUmbrella(jsonOut, umb.URL, created, len(subs)-len(specs))
}

// scanExisting returns the set of normalized issue refs that already have a
// task, and whether the umbrella tracker task already exists.
func scanExisting(s *task.Manager, umbrellaURL string) (map[string]bool, bool) {
	refs := map[string]bool{}
	trackerExists := false
	umbKey := umbrella.NormalizeIssueRef(umbrellaURL)
	tasks, err := s.List()
	if err != nil {
		return refs, false
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Issue != "" {
			refs[umbrella.NormalizeIssueRef(t.Issue)] = true
		}
		if t.TaskType == task.TaskTypeUmbrella && umbrella.NormalizeIssueRef(t.Issue) == umbKey {
			trackerExists = true
		}
	}
	return refs, trackerExists
}

// materializeUmbrella creates the tracker (when absent) and one blocked child
// task per spec. It returns the number of child tasks created.
func materializeUmbrella(s *task.Manager, umb github.Issue, repo string, specs []umbrella.ChildSpec, byRef map[string]github.Issue, trackerExists bool) (int, error) {
	if !trackerExists {
		if _, err := s.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
			Issue:     task.Ptr(umb.URL),
			TaskType:  task.Ptr(task.TaskTypeUmbrella),
			ProjectID: task.Ptr(repo),
			Status:    task.Ptr(task.StatusInProgress),
			Tags:      task.Ptr([]string{"umbrella"}),
		}); err != nil {
			return 0, fmt.Errorf("create tracker: %w", err)
		}
	}

	created := 0
	for _, spec := range specs {
		if _, err := s.CreateFull(spec.Title, spec.Body, task.AgentModeHeadless, task.Update{
			Issue:         task.Ptr(spec.Issue),
			UmbrellaIssue: task.Ptr(umb.URL),
			DependsOn:     task.Ptr(canonicalizeDeps(spec.DependsOn, byRef)),
			ProjectID:     task.Ptr(repo),
			Status:        task.Ptr(task.StatusBlocked),
			Tags:          task.Ptr(childTags(spec.Issue, byRef)),
		}); err != nil {
			return created, fmt.Errorf("create child for %s: %w", spec.Issue, err)
		}
		created++
	}
	return created, nil
}

// canonicalizeDeps rewrites each dependency ref to the canonical issue URL of
// the sub-issue it points at, so a child's DependsOn matches the dependency
// task's Issue field exactly. An unknown ref (should not happen post-validate)
// is kept as-is.
func canonicalizeDeps(deps []string, byRef map[string]github.Issue) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if iss, ok := byRef[umbrella.NormalizeIssueRef(d)]; ok {
			out = append(out, iss.URL)
		} else {
			out = append(out, d)
		}
	}
	return out
}

// childTags returns the gating marker plus the sub-issue's own labels.
func childTags(ref string, byRef map[string]github.Issue) []string {
	tags := []string{umbrella.GatedTag}
	if iss, ok := byRef[umbrella.NormalizeIssueRef(ref)]; ok {
		for _, l := range iss.Labels {
			if !slices.Contains(tags, l) {
				tags = append(tags, l)
			}
		}
	}
	return tags
}

func reportUmbrella(jsonOut bool, umbrellaURL string, created, skipped int) int {
	if jsonOut {
		out, _ := json.Marshal(map[string]any{
			"umbrella": umbrellaURL,
			"created":  created,
			"skipped":  skipped,
		})
		fmt.Println(string(out))
		return 0
	}
	fmt.Printf("Expanded %s: created %d child task(s), %d already present.\n", umbrellaURL, created, skipped)
	return 0
}

// parseIssueURL extracts "owner/repo" and the issue number from a GitHub issue
// URL.
func parseIssueURL(raw string) (repo string, number int, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return "", 0, fmt.Errorf("not a GitHub issue URL: %s", raw)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", 0, fmt.Errorf("invalid issue number in %s", raw)
	}
	return parts[0] + "/" + parts[1], n, nil
}

// claudePlannerRunner returns a planner Runner that shells out to the claude
// CLI for a single structured-output completion. The planner reasons over text
// passed in the prompt and needs no tools.
func claudePlannerRunner(model string) umbrella.Runner {
	return func(ctx context.Context, prompt string) (string, error) {
		cmdArgs := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
		if model != "" {
			cmdArgs = append(cmdArgs, "--model", model)
		}
		out, err := exec.CommandContext(ctx, "claude", cmdArgs...).Output()
		if err != nil {
			return "", fmt.Errorf("run claude: %w", err)
		}
		return string(out), nil
	}
}
