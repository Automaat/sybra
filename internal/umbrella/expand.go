package umbrella

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

// errSkipUpdate signals tagTrackerDegraded's UpdateFn callback that the tag
// is already present, so the read-modify-write should short-circuit without
// writing (and without firing a task:updated event).
var errSkipUpdate = errors.New("skip update")

// PlannerAttemptTimeout is the floor of the per-attempt planner budget. It is
// passed to llmjob.Run as Spec.AttemptTimeout so every repair attempt gets its
// budget fresh, instead of splitting a single shared deadline across them
// (see #1555). The effective budget scales with prompt size via
// plannerAttemptTimeout: a fixed cap starves large umbrellas — a 38-child
// umbrella's decompose was deadline-killed on every attempt at a fixed 4m,
// looping the expansion forever (see #1570).
const PlannerAttemptTimeout = 4 * time.Minute

// plannerAttemptTimeoutMax caps the scaled per-attempt budget so one attempt
// on a pathologically large umbrella cannot hold the expansion slot for an
// hour.
const plannerAttemptTimeoutMax = 30 * time.Minute

// plannerAttemptPromptChunk is how much prompt buys one extra minute of
// per-attempt budget on top of the PlannerAttemptTimeout floor.
const plannerAttemptPromptChunk = 2 << 10

// plannerAttemptTimeout returns the per-attempt planner budget for a prompt of
// promptLen bytes. Model time grows with both the prompt to read and the
// answer to emit, and the answer (one change-surface JSON entry per sub-issue)
// grows with the prompt listing them — so the budget scales with input size
// rather than sub-issue count, which FallbackPlannerRunner cannot see.
func plannerAttemptTimeout(promptLen int) time.Duration {
	d := PlannerAttemptTimeout + time.Duration(promptLen/plannerAttemptPromptChunk)*time.Minute
	return min(d, plannerAttemptTimeoutMax)
}

// plannerJobSpec is the llmjob.Spec FallbackPlannerRunner runs the
// "umbrella-order" job with. It is shared with plannerTimeout below via
// Attempts() so the two can never drift out of sync the way a
// hand-maintained attempt-count constant could.
var plannerJobSpec = llmjob.Spec[Plan]{
	Name:           "umbrella-order",
	Tier:           llmjob.Standard,
	AttemptTimeout: PlannerAttemptTimeout,
}

// plannerJobAttempts mirrors llmjob's 1+maxRepairs attempts for plannerJobSpec
// — used only to size plannerTimeout below.
var plannerJobAttempts = plannerJobSpec.Attempts()

// plannerGenerateSamples is the maximum number of planner samples Generate can
// request: plannerAttempts for the initial attemptPlan plus another
// plannerAttempts for the critic re-ask path when the first valid plan looks
// suspiciously flat.
const plannerGenerateSamples = plannerAttempts * 2

// plannerTimeout bounds the whole Generate call — every attemptPlan retry
// plus the zero-edge-floor critic re-ask — so a hung process cannot wedge an
// expansion indefinitely. It covers plannerGenerateSamples each getting
// plannerJobAttempts full PlannerAttemptTimeout floor slices, plus per-sub
// headroom sized so the prompt-scaled attempt budgets of a large umbrella
// (see plannerAttemptTimeout) still fit: a bigger umbrella means a longer
// prompt and legitimately more model time, and a bigger expansion is more
// expensive to have starved.
func plannerTimeout(subCount int) time.Duration {
	return PlannerAttemptTimeout*time.Duration(plannerJobAttempts*plannerGenerateSamples) +
		time.Duration(subCount*plannerGenerateSamples)*15*time.Second
}

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

// expandConfig holds the optional settings ExpandOption values apply.
type expandConfig struct {
	lister  TrackedFilesFunc
	minSubs int
}

// ExpandOption configures an optional Expand behavior.
type ExpandOption func(*expandConfig)

// WithExpandGrounder threads a tracked-file lister into the planner's
// grounding step (see WithGrounder). Omitting this option leaves Expand's
// behavior unchanged (today's LLM-only touches).
func WithExpandGrounder(lister TrackedFilesFunc, minSubs int) ExpandOption {
	return func(c *expandConfig) {
		c.lister = lister
		c.minSubs = minSubs
	}
}

// Result summarizes an expansion.
type Result struct {
	UmbrellaURL string
	Created     int  // child tasks created this run
	Skipped     int  // sub-issues already materialized or done
	Degraded    bool // true when the DAG came from linearChainFallback, not the model
}

// Expand fetches a GitHub umbrella issue's native sub-issues, runs the planner
// to extract a dependency DAG, and materializes one `umbrella` tracker task
// plus one `blocked`+gated child per open sub-issue. It is idempotent: only
// sub-issues without an existing task are created, and a fully-materialized
// re-run skips the planner entirely. The planner run is bounded by
// plannerTimeout, scaled to the sub-issue count. Shared by the `sybra-cli
// umbrella` command and the GitHub issue fetcher's auto-detect path.
func Expand(ctx context.Context, tasks *task.Manager, run Runner, issueURL string, opts ...ExpandOption) (Result, error) {
	var cfg expandConfig
	for _, opt := range opts {
		opt(&cfg)
	}
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

	existing, trackerExists, trackerID, err := scanExisting(tasks, umb.URL)
	if err != nil {
		return Result{}, fmt.Errorf("scan existing tasks: %w", err)
	}
	// Short-circuit a full re-run: nothing to create means no (costly,
	// stochastic) planner call.
	if trackerExists && allMaterialized(planSubs, existing) {
		return Result{UmbrellaURL: umb.URL, Skipped: len(subs)}, nil
	}

	var genOpts []GenerateOption
	if cfg.lister != nil {
		genOpts = append(genOpts, WithGrounder(cfg.lister, cfg.minSubs))
	}

	pctx, cancel := context.WithTimeout(ctx, plannerTimeout(len(subs)))
	defer cancel()
	plan, err := Generate(pctx, run, umb.URL, umb.Body, planSubs, genOpts...)
	if err != nil {
		return Result{}, fmt.Errorf("plan umbrella: %w", err)
	}

	specs := ChildSpecs(plan, planSubs, existing)
	created, err := materialize(tasks, umb, specs, byRef, trackerExists, trackerID, plan.MaxParallel, plan.Fallback)
	if err != nil {
		return Result{}, err
	}
	return Result{UmbrellaURL: umb.URL, Created: created, Skipped: len(subs) - created, Degraded: plan.Fallback}, nil
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
// task, whether the umbrella tracker exists, and its task id when it does. A
// List failure is propagated so the caller aborts rather than treating an
// unreadable store as empty and creating a duplicate DAG.
func scanExisting(tasks *task.Manager, umbrellaURL string) (refs map[string]bool, trackerExists bool, trackerID string, err error) {
	all, err := tasks.List()
	if err != nil {
		return nil, false, "", err
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
			trackerID = t.ID
		}
	}
	return refs, trackerExists, trackerID, nil
}

// materialize creates the tracker (when absent) and one gated todo child per
// spec. When degraded (the plan came from linearChainFallback), the tracker
// carries FallbackTag so a systematically-failing planner is board-visible:
// on a fresh tracker the tag is included at creation; on re-expansion against
// an already-materialized tracker it is appended idempotently (add-if-absent)
// via UpdateFn, since a partial re-expansion can hit the fallback on a
// tracker created by an earlier, successful run.
func materialize(tasks *task.Manager, umb github.Issue, specs []ChildSpec, byRef map[string]github.Issue, trackerExists bool, trackerID string, maxParallel int, degraded bool) (int, error) {
	if !trackerExists {
		tags := []string{"umbrella", MaxParallelTag(maxParallel)}
		if degraded {
			tags = append(tags, FallbackTag)
		}
		if _, err := tasks.CreateFull(umb.Title, umb.Body, task.AgentModeHeadless, task.Update{
			Issue:     task.Ptr(umb.URL),
			TaskType:  task.Ptr(task.TaskTypeUmbrella),
			ProjectID: task.Ptr(umb.Repository),
			Status:    task.Ptr(task.StatusInProgress),
			Tags:      task.Ptr(tags),
		}); err != nil {
			return 0, fmt.Errorf("create tracker: %w", err)
		}
	} else if degraded {
		if err := tagTrackerDegraded(tasks, trackerID); err != nil {
			return 0, err
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

// tagTrackerDegraded appends FallbackTag to an already-materialized tracker
// if it isn't there yet. Reads first and skips the write when the tag is
// already present, so a repeated degraded re-expansion against the same
// tracker doesn't emit a spurious task:updated event.
func tagTrackerDegraded(tasks *task.Manager, trackerID string) error {
	_, err := tasks.UpdateFn(trackerID, func(cur task.Task) (task.Update, error) {
		if slices.Contains(cur.Tags, FallbackTag) {
			return task.Update{}, errSkipUpdate
		}
		return task.Update{
			Tags: task.Ptr(append(slices.Clone(cur.Tags), FallbackTag)),
		}, nil
	})
	if err != nil {
		if errors.Is(err, errSkipUpdate) {
			return nil
		}
		return fmt.Errorf("tag tracker degraded: %w", err)
	}
	return nil
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

// FallbackPlannerRunner returns a planner Runner that prefers Claude and falls
// back to another CLI provider when the preferred provider is unavailable.
func FallbackPlannerRunner(model string, gates ...provider.HealthGate) Runner {
	var gate provider.HealthGate
	if len(gates) > 0 {
		gate = gates[0]
	}
	return func(ctx context.Context, prompt string) (string, error) {
		spec := plannerJobSpec
		spec.AttemptTimeout = plannerAttemptTimeout(len(prompt))
		plan, _, err := llmjob.Run(ctx, prompt, spec, llmexec.Options{Gate: gate, Models: claudeModelOverride(model)})
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(plan)
		if err != nil {
			return "", fmt.Errorf("marshal planner result: %w", err)
		}
		return string(out), nil
	}
}

func claudeModelOverride(model string) map[string]string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return map[string]string{"claude": model}
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
