package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Definition is a declarative workflow stored as YAML.
type Definition struct {
	ID          string    `yaml:"id" json:"id"`
	Name        string    `yaml:"name" json:"name"`
	Description string    `yaml:"description,omitempty" json:"description"`
	Trigger     Trigger   `yaml:"trigger" json:"trigger"`
	Steps       []Step    `yaml:"steps" json:"steps"`
	Builtin     bool      `yaml:"builtin,omitempty" json:"builtin"`
	CreatedAt   time.Time `yaml:"created_at,omitempty" json:"createdAt"`
	UpdatedAt   time.Time `yaml:"updated_at,omitempty" json:"updatedAt"`
}

// StepByID returns the step with the given ID, or nil. Recurses into
// `parallel` blocks so children of a parallel step are also reachable.
func (d *Definition) StepByID(id string) *Step {
	for i := range d.Steps {
		if s := findStepRecursive(&d.Steps[i], id); s != nil {
			return s
		}
	}
	return nil
}

// findStepRecursive walks the step tree (including Parallel children).
func findStepRecursive(s *Step, id string) *Step {
	if s.ID == id {
		return s
	}
	for i := range s.Parallel {
		if c := findStepRecursive(&s.Parallel[i], id); c != nil {
			return c
		}
	}
	return nil
}

// FirstStep returns the first step, or nil if empty.
func (d *Definition) FirstStep() *Step {
	if len(d.Steps) == 0 {
		return nil
	}
	return &d.Steps[0]
}

// Trigger defines when a workflow activates.
// Priority breaks ties when multiple definitions match the same event: higher
// wins, zero is the default. Ties at the same priority fall back to
// alphabetical order by workflow ID for determinism.
type Trigger struct {
	On         string      `yaml:"on" json:"on"` // "task.created", "task.status_changed", "pr.event"
	Priority   int         `yaml:"priority,omitempty" json:"priority"`
	Conditions []Condition `yaml:"conditions,omitempty" json:"conditions"`

	// Position stores x,y for the graph editor (not used by engine).
	Position *Position `yaml:"position,omitempty" json:"position"`
}

// Condition is a field-operator-value check.
type Condition struct {
	Field    string `yaml:"field" json:"field"`       // "task.tags", "task.status", "task.agent_mode"
	Operator string `yaml:"operator" json:"operator"` // "equals", "not_equals", "contains", "not_contains", "exists", "in", "not_in"
	Value    string `yaml:"value" json:"value"`
}

// StepType enumerates the kinds of workflow steps.
type StepType string

const (
	StepRunAgent             StepType = "run_agent"
	StepWaitHuman            StepType = "wait_human"
	StepSetStatus            StepType = "set_status"
	StepCondition            StepType = "condition"
	StepShell                StepType = "shell"
	StepEnsurePRClosesIssue  StepType = "ensure_pr_closes_issue"
	StepStampPRAttribution   StepType = "stamp_pr_attribution"
	StepRerequestReview      StepType = "rerequest_review"
	StepVerifyCommits        StepType = "verify_commits"
	StepLinkPRAndReview      StepType = "link_pr_and_review"
	StepEvaluate             StepType = "evaluate"
	StepRequireSidecar       StepType = "require_sidecar"
	StepValidatePlan         StepType = "validate_plan"
	StepValidatePlanContract StepType = "validate_plan_contract"
	StepTriageReview         StepType = "triage_review"
	// StepDetectTampering inspects the worktree diff for reward-hacking /
	// test-tampering signals (deleted assertions, added skip/xfail markers,
	// deleted test files, neutered CI). A high-severity finding flips the task
	// to human-required so it cannot reach done without an explicit human bless.
	StepDetectTampering StepType = "detect_tampering"
	// StepVerifyChecks runs the project's deterministic verify suite
	// (checks.verify, opt-in) on the agent's branch HEAD before review. A
	// non-zero exit flips the task to human-required so an implementation that
	// does not pass its own declared tests cannot reach a PR. Complements
	// detect_tampering: structural test tampering is caught there; this catches
	// incomplete/broken work committed without the suite passing.
	StepVerifyChecks StepType = "verify_checks"
	// StepRoutePRFixResult inspects the completed pr-fix agent output before
	// mechanical PR relinking can overwrite a human-required escalation.
	StepRoutePRFixResult StepType = "route_pr_fix_result"
	// StepRouteTestResult reads the test-runner verdict and routes the task:
	// pass → ready-pr, fail → in-progress (re-implement) until the attempt
	// cap is hit, then human-required.
	StepRouteTestResult StepType = "route_test_result"
	// StepParallel runs its `Parallel` children concurrently as run_agent
	// steps. The parent step advances only after every child has terminated;
	// parent-level Next is evaluated against the parent step record (with the
	// aggregate failure flag set when any child failed past its retry budget).
	StepParallel StepType = "parallel"
	// StepSyncBranch proactively reconciles the task's worktree branch with
	// the project's default branch before the workflow reaches a PR handoff
	// point, shrinking the window in which the branch can go stale. Always
	// non-blocking: conflict, failure, or missing-worktree/syncer outcomes are
	// recorded but never flip the task to human-required — the workflow
	// continues to its next step regardless of outcome.
	StepSyncBranch StepType = "sync_branch"
	// StepResumeWorkflow is the terminal step of a recovery workflow
	// (branch-conflict-fix) that re-enters the task's original interrupted
	// workflow/stage. Reads resume_workflow_id/resume_workflow_vars/
	// resume_status vars captured before the recovery workflow was started;
	// a missing resume_workflow_id is a no-op (the workflow simply ends, and
	// normal status-driven cascade dispatch — see
	// completion.Handler.OnWorkflowComplete — picks up whatever workflow
	// matches the restored task status).
	StepResumeWorkflow StepType = "resume_workflow"
	// StepBestOfN runs Config.Attempts implementation agents concurrently,
	// each in its OWN isolated worktree/branch (unlike StepParallel, whose
	// children share one checkout) — see internal/worktree.Manager.PrepareAttempt.
	// The parent step advances only once every attempt has terminated;
	// zero or exactly one successful attempt fails closed to human-required
	// instead of proceeding to judging (see engine_advance.go).
	StepBestOfN StepType = "best_of_n"
	// StepPromoteBestOfN is the mechanical (no-LLM) step that reads the
	// mandatory JudgeStep's output, mechanically validates the winner JSON,
	// and fast-forwards the canonical task branch/worktree onto the winning
	// attempt via internal/worktree.Manager.PromoteAttempt. Malformed/
	// ambiguous/unknown judge output and unsafe promotion each fail closed to
	// human-required with a distinct reason (see engine_steps_bestofn.go).
	StepPromoteBestOfN StepType = "promote_best_of_n"
	// StepPushBranch deterministically pushes the task's worktree branch to
	// its existing PR (git plumbing only — internal/project.PushSync). No
	// LLM/agent involved; replaces the push-branch agent role.
	StepPushBranch StepType = "push_branch"
	// StepCreatePR deterministically pushes the task's worktree branch and
	// opens a GitHub PR for it (git plumbing + `gh pr create`), drafting the
	// title/body via a single cheap LLM job (internal/prcontent). No agent
	// session involved; replaces the create-pr agent role.
	StepCreatePR StepType = "create_pr"
	// StepClassifyTask deterministically runs the Go triage classifier
	// (internal/triage) against the task and applies its verdict — no agent
	// session involved. Replaces a run_agent step that wrapped a full Sonnet
	// agent invoking the /sybra-triage skill around this same classifier.
	StepClassifyTask StepType = "classify_task"
)

// Best-of-N attempt count bounds enforced by Definition.Validate. A floor of
// 2 makes "best-of-N" meaningful (a single attempt has nothing to judge); the
// cap bounds worst-case fan-out cost/dispatch time per task.
const (
	bestOfNMinAttempts = 2
	bestOfNMaxAttempts = 6
)

// Step is one node in the workflow graph.
type Step struct {
	ID       string       `yaml:"id" json:"id"`
	Name     string       `yaml:"name" json:"name"`
	Type     StepType     `yaml:"type" json:"type"`
	Config   StepConfig   `yaml:"config" json:"config"`
	Next     []Transition `yaml:"next,omitempty" json:"next"`
	Parallel []Step       `yaml:"parallel,omitempty" json:"parallel"`

	// Position stores x,y for the graph editor (not used by engine).
	Position *Position `yaml:"position,omitempty" json:"position"`
}

// Position stores node coordinates for the visual editor.
type Position struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
}

// StepConfig holds type-specific configuration.
// Only fields relevant to the step type are populated.
type StepConfig struct {
	// run_agent
	Role          string   `yaml:"role,omitempty" json:"role"`
	Mode          string   `yaml:"mode,omitempty" json:"mode"`
	Model         string   `yaml:"model,omitempty" json:"model"`
	Provider      string   `yaml:"provider,omitempty" json:"provider"` // "", "claude", "codex", "copilot", "cross"
	Prompt        string   `yaml:"prompt,omitempty" json:"prompt"`
	AllowedTools  []string `yaml:"allowed_tools,omitempty" json:"allowedTools"`
	NeedsWorktree bool     `yaml:"needs_worktree,omitempty" json:"needsWorktree"`

	// wait_human
	HumanActions []string `yaml:"human_actions,omitempty" json:"humanActions"`

	// set_status
	Status       string `yaml:"status,omitempty" json:"status"`
	StatusReason string `yaml:"status_reason,omitempty" json:"statusReason"`

	// condition
	Check *Condition `yaml:"check,omitempty" json:"check"`

	// run_agent: retry + reuse
	MaxRetries int  `yaml:"max_retries,omitempty" json:"maxRetries"`
	ReuseAgent bool `yaml:"reuse_agent,omitempty" json:"reuseAgent"`

	// run_agent: treat the step as complete when the task status transitions
	// to this value. Required for interactive/conversational agents that
	// never exit on their own — the workflow advances on status change
	// instead of process exit.
	WaitForStatus string `yaml:"wait_for_status,omitempty" json:"waitForStatus"`

	// shell
	Command string `yaml:"command,omitempty" json:"command"`
	Dir     string `yaml:"dir,omitempty" json:"dir"`

	// require_sidecar: which sidecar must be non-empty for the step to pass.
	// Valid values: "plan", "plan_critique", "plan_research",
	// "plan_decisions", "plan_brief", "code_review", or "plan_draft.<name>".
	Sidecar string `yaml:"sidecar,omitempty" json:"sidecar"`
	// AllowMissing turns require_sidecar into a soft gate: the step records a
	// warning output instead of flipping the task to human-required.
	AllowMissing bool `yaml:"allow_missing,omitempty" json:"allowMissing"`

	// run_agent: when set, the engine ingests a file produced by the agent
	// (typically under /tmp) and stores its content as the named task
	// sidecar after the agent exits. Lets review/critique steps work with
	// codex agents whose sandbox blocks writes outside the worktree —
	// the engine runs on the host with full filesystem access and closes
	// the gap. Read failures are logged, not fatal: the require_sidecar
	// guard still flips the task to human-required when the sidecar ends
	// up empty, preserving the existing safety net.
	ImportSidecar *ImportSidecar `yaml:"import_sidecar,omitempty" json:"importSidecar,omitempty"`
	// ImportSidecars is the plural form for a step that produces multiple
	// planning artifacts in one agent run. ImportSidecar remains supported for
	// existing workflows and editor compatibility.
	ImportSidecars []ImportSidecar `yaml:"import_sidecars,omitempty" json:"importSidecars"`

	// run_agent (codex only): inline JSON Schema enforced on the model's final
	// message via `codex exec --output-schema`. Ignored by claude/copilot, which
	// use prompt-driven structured output. Empty = no enforcement.
	OutputSchema string `yaml:"output_schema,omitempty" json:"outputSchema,omitempty"`

	// best_of_n: number of implementation attempts to fan out, each in its
	// own isolated worktree. Must be within [bestOfNMinAttempts, bestOfNMaxAttempts].
	Attempts int `yaml:"attempts,omitempty" json:"attempts,omitempty"`
	// best_of_n: providers assigned to attempts in index order, cycling if
	// there are fewer entries than Attempts. An empty list (or an empty
	// entry) uses the engine's default provider resolution for that attempt,
	// same as an unset run_agent Provider.
	AttemptProviders []string `yaml:"attempt_providers,omitempty" json:"attemptProviders,omitempty"`

	// promote_best_of_n: step ID of the prior run_agent judge step whose
	// output carries the mechanically-validated winner JSON
	// (`{winner_attempt_id, scores[], rationale}`). Required.
	JudgeStep string `yaml:"judge_step,omitempty" json:"judgeStep,omitempty"`
	// promote_best_of_n: step ID of the prior best_of_n step whose attempts
	// the judge scored. Required — resolves the attempt worktree/branch the
	// winner_attempt_id names.
	BestOfNStep string `yaml:"best_of_n_step,omitempty" json:"bestOfNStep,omitempty"`

	// run_agent: enforce the cumulative task cost budget before dispatching.
	// Set on direct-dispatch steps (e.g. the best-of-N judge, which passes a
	// pre-staged dir and so bypasses StartAgentWithAssignment's own budget
	// enforcement) so the run fails closed to human-required instead of
	// spending on the step when the task is already over budget.
	BudgetPreflight bool `yaml:"budget_preflight,omitempty" json:"budgetPreflight,omitempty"`
}

// ImportSidecar describes a sidecar file the engine should ingest after a
// run_agent step succeeds.
type ImportSidecar struct {
	// Kind is the sidecar slot to populate.
	Kind string `yaml:"kind" json:"kind"`
	// From is a Go template path the engine renders against the run's
	// TemplateContext (e.g. {{getvar .Vars "_dir"}}/.sybra-review-{{.Task.ID}}.md).
	From string `yaml:"from" json:"from"`
	// Required flips the task to human-required when the imported file is
	// missing or empty. Existing optional sidecars keep the old log-only
	// behavior; new mandatory artifacts can fail closed without blocking
	// markdown-only migration tasks that never ran the producing step.
	Required bool `yaml:"required,omitempty" json:"required,omitempty"`
}

const maxRetries = 10

// Validate checks the definition for configuration errors.
func (d *Definition) Validate() error {
	seenIDs := make(map[string]bool, len(d.Steps))
	for i := range d.Steps {
		s := &d.Steps[i]
		if strings.Contains(s.ID, bestOfNAttemptSep) {
			return fmt.Errorf("step %q: step ids must not contain %q", s.ID, bestOfNAttemptSep)
		}
		if s.Config.MaxRetries > maxRetries {
			return fmt.Errorf("step %q: max_retries %d exceeds limit %d", s.ID, s.Config.MaxRetries, maxRetries)
		}
		if err := validateParallelStep(s, seenIDs); err != nil {
			return err
		}
		if err := validateBestOfNStep(s); err != nil {
			return err
		}
		if err := validatePromoteBestOfNStep(d, s); err != nil {
			return err
		}
		if seenIDs[s.ID] {
			return fmt.Errorf("step %q: duplicate id", s.ID)
		}
		seenIDs[s.ID] = true
	}
	if err := d.ValidateFields(); err != nil {
		return err
	}
	return nil
}

// validateParallelStep enforces that a `parallel` step has at least two
// run_agent children, no nested parallels, and globally-unique child IDs.
// The constraints exist because the engine's step bookkeeping (agentSteps
// map, ImportSidecar lookup, retry counter) is keyed by step ID — duplicates
// would cause cross-step state to clobber each other.
func validateParallelStep(s *Step, seenIDs map[string]bool) error {
	if s.Type != StepParallel {
		if len(s.Parallel) > 0 {
			return fmt.Errorf("step %q: parallel children only allowed when type is %q", s.ID, StepParallel)
		}
		return nil
	}
	if len(s.Parallel) < 2 {
		return fmt.Errorf("step %q: parallel step needs at least 2 children", s.ID)
	}
	for i := range s.Parallel {
		c := &s.Parallel[i]
		if c.Type != StepRunAgent {
			return fmt.Errorf("step %q: parallel child %q has type %q (only %q allowed)",
				s.ID, c.ID, c.Type, StepRunAgent)
		}
		if len(c.Parallel) > 0 {
			return fmt.Errorf("step %q: parallel child %q nests another parallel block (not supported)", s.ID, c.ID)
		}
		if c.Config.MaxRetries > maxRetries {
			return fmt.Errorf("step %q: child %q max_retries %d exceeds limit %d", s.ID, c.ID, c.Config.MaxRetries, maxRetries)
		}
		if c.ID == "" {
			return fmt.Errorf("step %q: parallel child at index %d missing id", s.ID, i)
		}
		if seenIDs[c.ID] {
			return fmt.Errorf("step %q: parallel child id %q already used elsewhere in workflow", s.ID, c.ID)
		}
		seenIDs[c.ID] = true
	}
	return nil
}

// validAttemptProviders lists the providers a best_of_n step may pin an
// attempt to. Matches the set run_agent's Provider field accepts, minus
// "cross" and "ab" — both require inspecting the workflow's own prior step
// history/A-B config, which is meaningless when picked per-attempt before any
// attempt has run.
var validAttemptProviders = map[string]bool{
	"":        true,
	"claude":  true,
	"codex":   true,
	"copilot": true,
}

// validateBestOfNStep enforces the best_of_n step's attempt-count bounds and
// attempt-provider values. No-op for every other step type.
func validateBestOfNStep(s *Step) error {
	if s.Type != StepBestOfN {
		return nil
	}
	if s.Config.Attempts < bestOfNMinAttempts || s.Config.Attempts > bestOfNMaxAttempts {
		return fmt.Errorf("step %q: best_of_n attempts %d out of range [%d, %d]",
			s.ID, s.Config.Attempts, bestOfNMinAttempts, bestOfNMaxAttempts)
	}
	for _, p := range s.Config.AttemptProviders {
		if !validAttemptProviders[p] {
			return fmt.Errorf("step %q: invalid attempt provider %q", s.ID, p)
		}
	}
	return nil
}

// validatePromoteBestOfNStep enforces that a promote_best_of_n step names a
// judge_step that exists and is itself a run_agent step (the one that emits
// the winner JSON promoteBestOfN mechanically validates). No-op for every
// other step type.
func validatePromoteBestOfNStep(d *Definition, s *Step) error {
	if s.Type != StepPromoteBestOfN {
		return nil
	}
	if s.Config.JudgeStep == "" {
		return fmt.Errorf("step %q: promote_best_of_n requires judge_step", s.ID)
	}
	judge := d.StepByID(s.Config.JudgeStep)
	if judge == nil {
		return fmt.Errorf("step %q: judge_step %q not found", s.ID, s.Config.JudgeStep)
	}
	if judge.Type != StepRunAgent {
		return fmt.Errorf("step %q: judge_step %q must be a run_agent step (got %q)", s.ID, s.Config.JudgeStep, judge.Type)
	}
	if s.Config.BestOfNStep == "" {
		return fmt.Errorf("step %q: promote_best_of_n requires best_of_n_step", s.ID)
	}
	bestOfN := d.StepByID(s.Config.BestOfNStep)
	if bestOfN == nil {
		return fmt.Errorf("step %q: best_of_n_step %q not found", s.ID, s.Config.BestOfNStep)
	}
	if bestOfN.Type != StepBestOfN {
		return fmt.Errorf("step %q: best_of_n_step %q must be a best_of_n step (got %q)", s.ID, s.Config.BestOfNStep, bestOfN.Type)
	}
	return nil
}

// ValidateFields returns an error listing any trigger or transition field
// references that the engine will never populate, plus any enum-value
// comparisons that pick values outside the known enum set. Catches two
// classes of dead workflow condition at load/save time:
//
//  1. A trigger on "project.type" with no caller supplying that key (the
//     auto-merge dead-code shape).
//  2. A trigger on "pr.issue_kind" comparing against "ci-failure" (dash)
//     while the constant emits "ci_failure" (underscore) — the original
//     dispatch-mismatch shape.
func (d *Definition) ValidateFields() error {
	acc := fieldValidationAcc{
		unknown:      map[string]bool{},
		badValues:    map[string][]string{},
		badOperators: map[string][]string{},
	}
	for i := range d.Trigger.Conditions {
		collectUnknownCondition(&d.Trigger.Conditions[i], &acc)
	}
	for i := range d.Steps {
		collectUnknownStepFields(&d.Steps[i], &acc)
	}
	if len(acc.unknown) == 0 && len(acc.badValues) == 0 && len(acc.badOperators) == 0 {
		return nil
	}
	var parts []string
	if len(acc.unknown) > 0 {
		names := make([]string, 0, len(acc.unknown))
		for k := range acc.unknown {
			names = append(names, k)
		}
		sort.Strings(names)
		parts = append(parts, "unknown field(s): "+strings.Join(names, ", "))
	}
	if len(acc.badValues) > 0 {
		fields := make([]string, 0, len(acc.badValues))
		for k := range acc.badValues {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, f := range fields {
			vals := acc.badValues[f]
			sort.Strings(vals)
			parts = append(parts, fmt.Sprintf("invalid %s value(s): %s", f, strings.Join(vals, ",")))
		}
	}
	if len(acc.badOperators) > 0 {
		fields := make([]string, 0, len(acc.badOperators))
		for k := range acc.badOperators {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, f := range fields {
			ops := acc.badOperators[f]
			sort.Strings(ops)
			parts = append(parts, fmt.Sprintf(
				"operator %s not allowed on enum field %s (use equals/in)",
				strings.Join(ops, ","), f))
		}
	}
	return fmt.Errorf("workflow %q: %s", d.ID, strings.Join(parts, "; "))
}

// fieldValidationAcc accumulates ValidateFields results while walking the
// trigger and step tree. Grouped to keep the recursive collectors' signatures
// from growing a new parameter every time a new class of error gets added.
type fieldValidationAcc struct {
	unknown      map[string]bool
	badValues    map[string][]string
	badOperators map[string][]string
}

func collectUnknownCondition(c *Condition, acc *fieldValidationAcc) {
	if !isKnownField(c.Field) {
		acc.unknown[c.Field] = true
		return
	}
	if checkEnumOperator(c.Field, c.Operator) {
		acc.badOperators[c.Field] = append(acc.badOperators[c.Field], c.Operator)
		return
	}
	if bad := checkEnumValue(c.Field, c.Operator, c.Value); len(bad) > 0 {
		acc.badValues[c.Field] = append(acc.badValues[c.Field], bad...)
	}
}

func collectUnknownStepFields(s *Step, acc *fieldValidationAcc) {
	if s.Config.Check != nil {
		collectUnknownCondition(s.Config.Check, acc)
	}
	for i := range s.Next {
		if s.Next[i].When != nil {
			collectUnknownCondition(s.Next[i].When, acc)
		}
	}
	for i := range s.Parallel {
		collectUnknownStepFields(&s.Parallel[i], acc)
	}
}

// Transition defines an edge from one step to another.
type Transition struct {
	When *Condition `yaml:"when,omitempty" json:"when"` // nil = default/fallback
	GoTo string     `yaml:"goto" json:"goto"`           // step ID; "" = end workflow
}
