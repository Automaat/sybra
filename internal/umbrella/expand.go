package umbrella

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// PlannerTimeout bounds a single planner LLM invocation so a hung process
// cannot wedge an expansion indefinitely.
const PlannerTimeout = 5 * time.Minute

// FetchTimeout bounds the GitHub sub-issue fetch so a stalled gh call cannot
// wedge the caller (notably the issue poll loop).
const FetchTimeout = 60 * time.Second

// fetchUmbrellaBounded fetches the umbrella under FetchTimeout, releasing the
// timer as soon as the fetch returns (defer right after WithTimeout).
func fetchUmbrellaBounded(ctx context.Context, repo string, number int) (umbrella github.Issue, subs []github.Issue, err error) {
	fctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	return github.FetchUmbrella(fctx, repo, number)
}

// Result summarizes an expansion.
type Result struct {
	UmbrellaURL string
	Created     int // child tasks created this run
	Skipped     int // sub-issues already materialized or done
}

// Expand fetches a GitHub umbrella issue's native sub-issues, runs the planner
// to extract a dependency DAG, and materializes one `umbrella` tracker task
// plus one `blocked`+gated child per open sub-issue. It is idempotent: only
// sub-issues without an existing task are created, and a fully-materialized
// re-run skips the planner entirely. The planner run is bounded by
// PlannerTimeout. Shared by the `sybra-cli umbrella` command and the GitHub
// issue fetcher's auto-detect path.
func Expand(ctx context.Context, tasks *task.Manager, run Runner, issueURL string) (Result, error) {
	repo, number, ok := ParseRef(issueURL)
	if !ok {
		return Result{}, fmt.Errorf("not a GitHub issue URL: %s", issueURL)
	}
	umb, subs, err := fetchUmbrellaBounded(ctx, repo, number)
	if err != nil {
		return Result{}, fmt.Errorf("fetch umbrella: %w", err)
	}
	if len(subs) == 0 {
		return Result{}, fmt.Errorf("umbrella %s has no sub-issues", issueURL)
	}

	// Index sub-issues by canonical (URL) ref for planner input + later lookup.
	planSubs := make([]SubIssue, len(subs))
	byRef := make(map[string]github.Issue, len(subs))
	for i := range subs {
		planSubs[i] = SubIssue{
			Ref:    subs[i].URL,
			Title:  subs[i].Title,
			Body:   subs[i].Body,
			Closed: strings.EqualFold(subs[i].State, "CLOSED"),
		}
		byRef[NormalizeIssueRef(subs[i].URL)] = subs[i]
	}

	existing, trackerExists, err := scanExisting(tasks, umb.URL)
	if err != nil {
		return Result{}, fmt.Errorf("scan existing tasks: %w", err)
	}
	// Short-circuit a full re-run: nothing to create means no (costly,
	// stochastic) planner call.
	if trackerExists && allMaterialized(planSubs, existing) {
		return Result{UmbrellaURL: umb.URL, Skipped: len(subs)}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, PlannerTimeout)
	defer cancel()
	plan, err := Generate(pctx, run, umb.URL, umb.Body, planSubs)
	if err != nil {
		return Result{}, fmt.Errorf("plan umbrella: %w", err)
	}

	specs := ChildSpecs(plan, planSubs, existing)
	created, err := materialize(tasks, umb, specs, byRef, trackerExists, plan.MaxParallel)
	if err != nil {
		return Result{}, err
	}
	return Result{UmbrellaURL: umb.URL, Created: created, Skipped: len(subs) - created}, nil
}

// allMaterialized reports whether every open sub-issue already has a task.
func allMaterialized(subs []SubIssue, existing map[string]bool) bool {
	for _, s := range subs {
		if s.Closed {
			continue
		}
		if !existing[NormalizeIssueRef(s.Ref)] {
			return false
		}
	}
	return true
}

// scanExisting returns the set of normalized issue refs that already have a
// task, and whether the umbrella tracker exists. A List failure is propagated
// so the caller aborts rather than treating an unreadable store as empty and
// creating a duplicate DAG.
func scanExisting(tasks *task.Manager, umbrellaURL string) (refs map[string]bool, trackerExists bool, err error) {
	all, err := tasks.List()
	if err != nil {
		return nil, false, err
	}
	refs = make(map[string]bool, len(all))
	umbKey := NormalizeIssueRef(umbrellaURL)
	for i := range all {
		t := &all[i]
		if t.Issue != "" {
			refs[NormalizeIssueRef(t.Issue)] = true
		}
		if t.TaskType == task.TaskTypeUmbrella && NormalizeIssueRef(t.Issue) == umbKey {
			trackerExists = true
		}
	}
	return refs, trackerExists, nil
}

// materialize creates the tracker (when absent) and one gated todo child per spec.
func materialize(tasks *task.Manager, umb github.Issue, specs []ChildSpec, byRef map[string]github.Issue, trackerExists bool, maxParallel int) (int, error) {
	if !trackerExists {
		if _, err := tasks.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
			Issue:     task.Ptr(umb.URL),
			TaskType:  task.Ptr(task.TaskTypeUmbrella),
			ProjectID: task.Ptr(umb.Repository),
			Status:    task.Ptr(task.StatusInProgress),
			Tags:      task.Ptr([]string{"umbrella", MaxParallelTag(maxParallel)}),
		}); err != nil {
			return 0, fmt.Errorf("create tracker: %w", err)
		}
	}

	created := 0
	for _, spec := range specs {
		if _, err := tasks.CreateFull(spec.Title, spec.Body, task.AgentModeHeadless, task.Update{
			Issue:         task.Ptr(spec.Issue),
			UmbrellaIssue: task.Ptr(umb.URL),
			DependsOn:     task.Ptr(canonicalizeDeps(spec.DependsOn, byRef)),
			ProjectID:     task.Ptr(childProjectID(spec.Issue, byRef, umb.Repository)),
			Status:        task.Ptr(task.StatusTodo),
			Tags:          task.Ptr(childTags(spec.Issue, byRef)),
		}); err != nil {
			return created, fmt.Errorf("create child for %s: %w", spec.Issue, err)
		}
		created++
	}
	return created, nil
}

// childProjectID returns the repo a child should be worked in: the sub-issue's
// own repository (sub-issues can live in a different repo than the umbrella),
// falling back to the umbrella's repo when unknown.
func childProjectID(ref string, byRef map[string]github.Issue, fallback string) string {
	if iss, ok := byRef[NormalizeIssueRef(ref)]; ok && iss.Repository != "" {
		return iss.Repository
	}
	return fallback
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
		if iss, ok := byRef[NormalizeIssueRef(d)]; ok {
			out = append(out, iss.URL)
		} else {
			out = append(out, d)
		}
	}
	return out
}

// childTags returns the gating marker plus the sub-issue's inheritable labels
// (load-bearing routing tags are filtered out so a child is not mis-routed).
func childTags(ref string, byRef map[string]github.Issue) []string {
	tags := []string{GatedTag}
	if iss, ok := byRef[NormalizeIssueRef(ref)]; ok {
		for _, l := range InheritableLabels(iss.Labels) {
			if !slices.Contains(tags, l) {
				tags = append(tags, l)
			}
		}
	}
	return tags
}

// ClaudePlannerRunner returns a planner Runner that shells out to the claude
// CLI for a single structured-output completion. The planner reasons over text
// in the prompt and needs no tools.
func ClaudePlannerRunner(model string) Runner {
	return func(ctx context.Context, prompt string) (string, error) {
		cmdArgs := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
		if model != "" {
			cmdArgs = append(cmdArgs, "--model", model)
		}
		cmd := exec.CommandContext(ctx, "claude", cmdArgs...)
		// Keep stdout clean for JSON parsing, but capture stderr so a planner
		// failure (or timeout-killed process) surfaces the real CLI message.
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return "", fmt.Errorf("run claude: %w: %s", err, msg)
			}
			return "", fmt.Errorf("run claude: %w", err)
		}
		return string(out), nil
	}
}

// IsUmbrellaIssue reports whether a GitHub issue should be auto-expanded as an
// umbrella: a ☂ title prefix (with or without the U+FE0F variation selector
// that renders it as emoji) or an "umbrella" label (case-insensitive, trimmed —
// GitHub preserves user-entered label casing/whitespace).
func IsUmbrellaIssue(title string, labels []string) bool {
	if strings.HasPrefix(strings.TrimSpace(title), "☂") {
		return true
	}
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), "umbrella") {
			return true
		}
	}
	return false
}
