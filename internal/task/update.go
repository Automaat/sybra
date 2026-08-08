package task

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/issueref"
	"github.com/Automaat/sybra/internal/reject"
	"github.com/Automaat/sybra/internal/workflow"
)

// Update carries optional field changes for Store.Update.
// A nil pointer means "leave unchanged"; a non-nil pointer applies the new value.
// For Workflow: nil = unchanged; non-nil = overwrite. A clear goes through ClearWorkflow instead: a non-nil Workflow holding a nil inner pointer works in-process but not over the API, so it is never the right encoding.
type Update struct {
	Title             *string
	Slug              *string
	Status            *Status
	StatusReason      *string
	ClearStatusReason *bool
	Escalation        *autonomy.EscalationReason
	AutonomyOutcome   *autonomy.Outcome
	Blocker           *blocker.State
	ClearBlocker      *bool
	// ClearWorkflow removes the task's workflow execution, and is the only encoding of a clear that survives JSON. Workflow is a **Execution: a non-nil outer pointer holding a nil inner one marshals to null, and unmarshal then nils the outer pointer, so a clear sent over the API read back as "leave unchanged".
	ClearWorkflow         *bool
	BlockedByIssue        *string
	UmbrellaIssue         *string
	DependsOn             *[]string
	DependsOnConditions   *[]DepCondition
	AgentMode             *string
	TaskType              *TaskType
	Body                  *string
	Tags                  *[]string
	ProjectID             *string
	Branch                *string
	WorktreeDir           *string
	HandoffSourceProvider *string
	PRNumber              *int
	Issue                 *string
	RefIssue              *string
	Reviewed              *bool
	RunRole               *string
	SupervisorSteer       *string
	ReviewPhase           *string
	ReviewedHeadSHA       *string
	ReviewedHeadAttempts  *int
	ReconcileFailures     *int
	PRPhase               *string
	Priority              *Priority
	DueDate               **time.Time
	Workflow              **workflow.Execution
	Plan                  *string
	PlanContract          *string
	PlanCritique          *string
	PlanResearch          *string
	PlanDecisions         *string
	PlanBrief             *string
	CodeReview            *string
	CurrentTestFailures   *string
	AcceptanceLedger      *string
	SpecDecision          *string
	CodeReviewVerdict     *string
	MaxTurns              *int
	ForkSubagent          *bool
	Sandbox               *bool
	SandboxOffReason      *string
	ReasoningEffort       *string
	Outcome               *string
	MergeCommit           *string
	TestingCycleStartedAt *time.Time
	Attachments           *[]Attachment
	EffectLog             *[]workflow.EffectRecord
}

func (u Update) writesSidecar() bool {
	return u.Plan != nil ||
		u.PlanContract != nil ||
		u.PlanCritique != nil ||
		u.PlanResearch != nil ||
		u.PlanDecisions != nil ||
		u.PlanBrief != nil ||
		u.CodeReview != nil ||
		u.CurrentTestFailures != nil ||
		u.AcceptanceLedger != nil ||
		u.SpecDecision != nil
}

// Ptr returns a pointer to v. Convenience for building Update literals.
func Ptr[T any](v T) *T {
	p := new(T)
	*p = v
	return p
}

// UpdateFromMap converts a map[string]any to a typed Update.
// Returns an error if any key is unknown or the value has the wrong type.
// This is the boundary adapter for CLI and Wails callers that receive raw maps.
func UpdateFromMap(raw map[string]any) (Update, error) {
	var u Update
	for k, v := range raw {
		if err := applyMapField(&u, k, v); err != nil {
			// Every failure here is the caller's own key or value, so mark
			// the whole boundary once rather than each field in turn.
			return Update{}, reject.New("%w", err)
		}
	}
	return u, nil
}

func applyMapField(u *Update, k string, v any) error {
	switch k {
	case "title", "slug", "status_reason", "blocked_by_issue", "umbrella_issue", "body",
		"project_id", "branch", "worktree_dir", "issue", "ref_issue", "run_role", "plan", "plan_critique",
		"plan_contract", "plan_research", "plan_decisions", "plan_brief", "code_review", "code_review_verdict",
		"current_test_failures", "acceptance_ledger", "spec_decision",
		"review_phase", "reviewed_head_sha", "pr_phase", "outcome", "merge_commit", "supervisor_steer":
		return applyPlainStringField(u, k, v)
	case "depends_on":
		return applyDependsOnField(u, k, v)
	case "depends_on_conditions":
		return applyDependsOnConditionsField(u, k, v)
	case "handoff_source_provider":
		return applyAgentProviderField(u, k, v)
	case "priority":
		return applyPriorityField(u, v)
	case "status":
		return applyStatusField(u, k, v)
	case "clear_status_reason", "clear_blocker":
		return applyClearField(u, k, v)
	case "agent_mode":
		return applyAgentModeField(u, k, v)
	case "task_type":
		return applyTaskTypeField(u, k, v)
	case "tags":
		return applyTagsField(u, k, v)
	case "pr_number":
		return applyPRNumberField(u, k, v)
	case "reviewed_head_attempts":
		return applyReviewedHeadAttemptsField(u, k, v)
	case "max_turns":
		return applyMaxTurnsField(u, v)
	case "fork_subagent":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("field %q: want bool, got %T", k, v)
		}
		u.ForkSubagent = &b
	case "sandbox":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("field %q: want bool, got %T", k, v)
		}
		u.Sandbox = &b
	case "sandbox_off_reason":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", k, v)
		}
		u.SandboxOffReason = &s
	case "reasoning_effort":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", k, v)
		}
		eff, err := ValidateReasoningEffort(s)
		if err != nil {
			return err
		}
		u.ReasoningEffort = &eff
	case "due_date":
		return applyDueDateField(u, v)
	case "reviewed":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("field %q: want bool, got %T", k, v)
		}
		u.Reviewed = &b
	case "workflow":
		wf, ok := v.(*workflow.Execution)
		if !ok {
			return fmt.Errorf("field %q: want *workflow.Execution, got %T", k, v)
		}
		u.Workflow = &wf
	case "blocker":
		return applyBlockerField(u, v)
	default:
		return fmt.Errorf("unknown task field %q", k)
	}
	return nil
}

func applyClearField(u *Update, k string, v any) error {
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("field %q: want bool, got %T", k, v)
	}
	switch k {
	case "clear_status_reason":
		u.ClearStatusReason = &b
	case "clear_blocker":
		u.ClearBlocker = &b
	default:
		return fmt.Errorf("unknown clear field %q", k)
	}
	return nil
}

// applyBlockerField builds a full-replacement blocker.State from a
// map[string]any (as opposed to a partial merge) — this matches how every
// blocker producer in internal/workflow and internal/sybra/review already
// writes the field: the whole state is authored together by whichever
// subsystem classified the blocking condition. Callers that want to change
// one attribute (e.g. flip Exhausted) must resend the full state, seeded from
// the task's current blocker if needed.
func applyBlockerField(u *Update, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("field %q: want object, got %T", "blocker", v)
	}
	var st blocker.State
	if raw, ok := m["kind"]; ok {
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", "blocker.kind", raw)
		}
		st.Kind = blocker.Kind(s)
	}
	if raw, ok := m["actor"]; ok {
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", "blocker.actor", raw)
		}
		st.Actor = blocker.Actor(s)
	}
	if raw, ok := m["code"]; ok {
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", "blocker.code", raw)
		}
		st.Code = s
	}
	if raw, ok := m["next_action"]; ok {
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", "blocker.next_action", raw)
		}
		st.NextAction = s
	}
	if raw, ok := m["retry_after"]; ok {
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("field %q: want string, got %T", "blocker.retry_after", raw)
		}
		if s != "" {
			parsed, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return fmt.Errorf("field %q: %w", "blocker.retry_after", err)
			}
			st.RetryAfter = &parsed
		}
	}
	if raw, ok := m["exhausted"]; ok {
		b, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("field %q: want bool, got %T", "blocker.exhausted", raw)
		}
		st.Exhausted = b
	}
	u.Blocker = &st
	return nil
}

func applyPlainStringField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	switch k {
	case "title":
		u.Title = &s
	case "slug":
		u.Slug = &s
	case "status_reason":
		u.StatusReason = &s
	case "blocked_by_issue":
		u.BlockedByIssue = &s
	case "umbrella_issue":
		u.UmbrellaIssue = &s
	case "body":
		u.Body = &s
	case "project_id":
		u.ProjectID = &s
	case "branch":
		u.Branch = &s
	case "worktree_dir":
		u.WorktreeDir = &s
	case "issue":
		u.Issue = &s
	case "ref_issue":
		u.RefIssue = &s
	case "run_role":
		u.RunRole = &s
	case "supervisor_steer":
		u.SupervisorSteer = &s
	case "plan":
		u.Plan = &s
	case "plan_contract":
		u.PlanContract = &s
	case "plan_critique":
		u.PlanCritique = &s
	case "plan_research":
		u.PlanResearch = &s
	case "plan_decisions":
		u.PlanDecisions = &s
	case "plan_brief":
		u.PlanBrief = &s
	case "code_review":
		u.CodeReview = &s
	case "current_test_failures":
		u.CurrentTestFailures = &s
	case "acceptance_ledger":
		u.AcceptanceLedger = &s
	case "spec_decision":
		u.SpecDecision = &s
	case "code_review_verdict":
		u.CodeReviewVerdict = &s
	case "review_phase":
		u.ReviewPhase = &s
	case "reviewed_head_sha":
		u.ReviewedHeadSHA = &s
	case "pr_phase":
		u.PRPhase = &s
	case "outcome":
		u.Outcome = &s
	case "merge_commit":
		u.MergeCommit = &s
	}
	return nil
}

func applyAgentProviderField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	prov, err := ValidateAgentProvider(strings.ToLower(strings.TrimSpace(s)))
	if err != nil {
		return err
	}
	u.HandoffSourceProvider = &prov
	return nil
}

func applyStatusField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	st, err := ValidateStatus(s)
	if err != nil {
		return err
	}
	u.Status = &st
	return nil
}

func applyAgentModeField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	mode, err := ValidateMintableAgentMode(s)
	if err != nil {
		return err
	}
	u.AgentMode = &mode
	return nil
}

func applyTaskTypeField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	tt, err := ValidateTaskType(s)
	if err != nil {
		return err
	}
	u.TaskType = &tt
	return nil
}

func applyTagsField(u *Update, k string, v any) error {
	switch tv := v.(type) {
	case []string:
		cp := make([]string, len(tv))
		copy(cp, tv)
		u.Tags = &cp
	case []any:
		parts, err := stringSlice(k, tv)
		if err != nil {
			return err
		}
		u.Tags = &parts
	case string:
		parts := strings.Split(tv, ",")
		u.Tags = &parts
	default:
		return fmt.Errorf("field %q: want []string or string, got %T", k, v)
	}
	return nil
}

// compactRefs trims and drops empty dependency refs. A blank ref is worse than a
// loud error: umbrella.Build skips empty keys, so depsSatisfied never finds one
// and the child task stays blocked forever. Applied to every shape — the
// comma-separated CLI shorthand guarded against this, but a JSON array
// ["t1", ""] reached the same field unguarded.
func compactRefs(in []string) []string {
	var out []string
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stringSlice coerces a JSON-decoded array. Every update that arrives over HTTP
// (the CLI writes that way, and so does the web GUI) decodes to []any, never
// []string — without this, any tags/depends_on update 500s.
func stringSlice(k string, in []any) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, e := range in {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("field %q: want a string element, got %T", k, e)
		}
		out = append(out, s)
	}
	return out, nil
}

func applyDependsOnField(u *Update, k string, v any) error {
	switch dv := v.(type) {
	case []string:
		parts := compactRefs(dv)
		u.DependsOn = &parts
	case []any:
		raw, err := stringSlice(k, dv)
		if err != nil {
			return err
		}
		parts := compactRefs(raw)
		u.DependsOn = &parts
	case string:
		parts := compactRefs(strings.Split(dv, ","))
		u.DependsOn = &parts
	default:
		return fmt.Errorf("field %q: want []string or string, got %T", k, v)
	}
	return nil
}

// applyDependsOnConditionsField parses a full-replacement DependsOnConditions
// list. This is the single validation boundary every caller goes through —
// the CLI's --depends-on-condition flag, the HTTP API, and the Wails
// binding — so a malformed or unknown Kind is rejected here at author time
// rather than reaching a task file (see task.DepConditionKindLabel/Note and
// holdUnmetConditions' fail-closed handling of the rare hand-edited-file case
// that still slips past this).
func applyDependsOnConditionsField(u *Update, k string, v any) error {
	switch dv := v.(type) {
	case []DepCondition:
		conds := slices.Clone(dv)
		if err := validateDepConditions(conds); err != nil {
			return err
		}
		u.DependsOnConditions = &conds
	case []any:
		conds := make([]DepCondition, 0, len(dv))
		for _, raw := range dv {
			m, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("field %q: want object element, got %T", k, raw)
			}
			c, err := depConditionFromMap(m)
			if err != nil {
				return err
			}
			conds = append(conds, c)
		}
		if err := validateDepConditions(conds); err != nil {
			return err
		}
		u.DependsOnConditions = &conds
	case nil:
		empty := []DepCondition{}
		u.DependsOnConditions = &empty
	default:
		return fmt.Errorf("field %q: want an array of {ref,kind,value} objects, got %T", k, v)
	}
	return nil
}

func depConditionFromMap(m map[string]any) (DepCondition, error) {
	ref, _ := m["ref"].(string)
	kind, _ := m["kind"].(string)
	value, _ := m["value"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DepCondition{}, fmt.Errorf("depends_on_conditions: ref is required")
	}
	if kind != DepConditionKindLabel && kind != DepConditionKindNote {
		return DepCondition{}, fmt.Errorf("depends_on_conditions: unknown kind %q for ref %q (valid: %s, %s)", kind, ref, DepConditionKindLabel, DepConditionKindNote)
	}
	if strings.TrimSpace(value) == "" {
		return DepCondition{}, fmt.Errorf("depends_on_conditions: value is required for ref %q", ref)
	}
	return DepCondition{Ref: ref, Kind: kind, Value: value}, nil
}

// validateDepConditions rejects more than one condition per Ref — the gate
// (holdUnmetConditions) only ever evaluates the first condition it finds for
// a given ref, so a silently-accepted second condition on the same ref would
// be silently inert rather than erroring where the mistake is made. The dedup
// key is normalized via issueref.Normalize so a URL and its "owner/repo#n"
// shorthand collapse to the same ref — the same equivalence matchesDepRef uses
// at gate time, otherwise two spellings of one dep slip past this check.
func validateDepConditions(conds []DepCondition) error {
	seen := make(map[string]bool, len(conds))
	for _, c := range conds {
		key := issueref.Normalize(c.Ref)
		if seen[key] {
			return fmt.Errorf("depends_on_conditions: duplicate ref %q — only one condition per ref is supported", c.Ref)
		}
		seen[key] = true
	}
	return nil
}

func applyReviewedHeadAttemptsField(u *Update, k string, v any) error {
	switch n := v.(type) {
	case int:
		u.ReviewedHeadAttempts = &n
	case float64:
		i := int(n)
		u.ReviewedHeadAttempts = &i
	default:
		return fmt.Errorf("field %q: want int or float64, got %T", k, v)
	}
	return nil
}

func applyPRNumberField(u *Update, k string, v any) error {
	switch n := v.(type) {
	case int:
		u.PRNumber = &n
	case float64:
		i := int(n)
		u.PRNumber = &i
	default:
		return fmt.Errorf("field %q: want int or float64, got %T", k, v)
	}
	return nil
}

func applyPriorityField(u *Update, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field \"priority\": want string, got %T", v)
	}
	p, err := ValidatePriority(s)
	if err != nil {
		return err
	}
	u.Priority = &p
	return nil
}

func applyDueDateField(u *Update, v any) error {
	if v == nil {
		var nilTime *time.Time
		u.DueDate = &nilTime
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field \"due_date\": want string or nil, got %T", v)
	}
	if s == "" {
		var nilTime *time.Time
		u.DueDate = &nilTime
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("field \"due_date\": invalid RFC3339 date %q: %w", s, err)
	}
	tp := &parsed
	u.DueDate = &tp
	return nil
}

func applyMaxTurnsField(u *Update, v any) error {
	var n int
	switch val := v.(type) {
	case int:
		n = val
	case float64:
		n = int(val)
	default:
		return fmt.Errorf("field \"max_turns\": want int, got %T", v)
	}
	if n < 0 {
		return fmt.Errorf("field \"max_turns\": must be >= 0, got %d", n)
	}
	u.MaxTurns = &n
	return nil
}
