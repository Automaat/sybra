package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/Automaat/sybra/internal/textutil"
)

const maxPlanContractBytes = 64 * 1024

// PlanContractSchemaV2 opts a plan contract into the admission-facts
// extension (Objective, RequiredCapabilities). An absent SchemaVersion (or
// explicit "1") validates under the original v1 rules only — existing
// contracts and in-flight tasks are unaffected by this migration, so
// deploying the extension never mass-flips them to human-required. See
// ValidatePlanContractForTask and execAdmissionPreflight
// (engine_steps_admission.go).
const PlanContractSchemaV2 = "2"

// planContractCapabilities is the closed registry of capability names a v2
// plan contract's RequiredCapabilities may declare. Deliberately closed (not
// free text): an unrecognized capability is a validation problem rather than
// a silent pass, so admission_preflight cannot be bypassed by inventing a new
// capability name the engine has no probe for.
var planContractCapabilities = map[string]bool{
	"repo_write": true,
	"network":    true,
	"provider":   true,
	"git_push":   true,
}

type PlanContract struct {
	TaskID             string                     `json:"task_id"`
	Branch             string                     `json:"branch"`
	Worktree           string                     `json:"worktree"`
	Files              []PlanContractFile         `json:"files"`
	Steps              []string                   `json:"steps"`
	ExpectedDeletions  []string                   `json:"expected_deletions,omitempty"`
	Verification       []PlanContractVerification `json:"verification"`
	AcceptanceCriteria []string                   `json:"acceptance_criteria"`
	RiskTier           string                     `json:"risk_tier"`
	PermissionTier     string                     `json:"permission_tier"`
	Rollback           string                     `json:"rollback"`
	// SchemaVersion gates the admission-facts fields below. Empty and "1" are
	// equivalent (the original schema); PlanContractSchemaV2 requires
	// Objective and validates RequiredCapabilities against a closed registry.
	// Any other value is an unsupported-schema-version validation problem.
	SchemaVersion string `json:"schema_version,omitempty"`
	// Objective is a one-line statement of what "done" means for this task,
	// required under PlanContractSchemaV2 — a machine-checkable admission
	// fact distinct from the free-form Steps/AcceptanceCriteria prose.
	Objective string `json:"objective,omitempty"`
	// Dependencies names other task IDs or external resources this task's
	// admission depends on. Advisory only (no validation beyond being a plain
	// string list) — scheduling on it is deferred, see #2466 Fix point 5.
	Dependencies []string `json:"dependencies,omitempty"`
	// RequiredCapabilities declares which planContractCapabilities entries
	// this task's implementation needs. Validated under PlanContractSchemaV2.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
}

type PlanContractFile struct {
	Path    string   `json:"path"`
	Purpose string   `json:"purpose,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
}

type PlanContractVerification struct {
	Command  string `json:"command,omitempty"`
	Manual   string `json:"manual,omitempty"`
	Expected string `json:"expected"`
}

func (e *Engine) execValidatePlanContract(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	raw := strings.TrimSpace(t.PlanContract)
	if raw == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "plan contract absent; markdown-only migration fallback"}, nil
	}
	if problems := ValidatePlanContractForTask(raw, taskID, t.Body); len(problems) > 0 {
		reason := "plan contract invalid: " + strings.Join(problems, "; ")
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.validate-plan-contract.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.validate-plan-contract.invalid", "task_id", taskID, "problems", strings.Join(problems, "; "))
		return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: "plan contract OK"}, nil
}

// ValidatePlanContract returns all schema and safety problems in a plan contract.
func ValidatePlanContract(raw, taskID string) []string {
	return ValidatePlanContractForTask(raw, taskID, "")
}

// ValidatePlanContractForTask returns all schema and safety problems in a plan
// contract, including source acceptance criteria coverage when the task body has
// an "Acceptance Criteria" section.
func ValidatePlanContractForTask(raw, taskID, taskBody string) []string {
	if len(raw) > maxPlanContractBytes {
		return []string{fmt.Sprintf("plan contract exceeds %d byte limit", maxPlanContractBytes)}
	}
	contract, err := parsePlanContract(raw)
	if err != nil {
		return []string{"malformed JSON: " + err.Error()}
	}

	var problems []string
	if strings.TrimSpace(contract.TaskID) == "" {
		problems = append(problems, "task_id is required")
	} else if contract.TaskID != taskID {
		problems = append(problems, fmt.Sprintf("task_id %q does not match current task %q", contract.TaskID, taskID))
	}
	if strings.TrimSpace(contract.Branch) == "" {
		problems = append(problems, "branch is required")
	}
	if strings.TrimSpace(contract.Worktree) == "" {
		problems = append(problems, "worktree is required")
	}
	if len(contract.Files) == 0 {
		problems = append(problems, "files must list at least one intended file")
	}
	for i, f := range contract.Files {
		if err := validateContractPath(f.Path); err != nil {
			problems = append(problems, fmt.Sprintf("files[%d].path: %v", i, err))
		}
	}
	if len(nonEmptyStrings(contract.Steps)) == 0 {
		problems = append(problems, "steps must include at least one implementation step")
	}
	for i, pattern := range contract.ExpectedDeletions {
		if _, _, err := normalizeExpectedDeletionEntry(pattern); err != nil {
			problems = append(problems, fmt.Sprintf("expected_deletions[%d]: %v", i, err))
		}
	}
	if len(contract.Verification) == 0 {
		problems = append(problems, "verification must include at least one command or manual check")
	}
	for i, v := range contract.Verification {
		if strings.TrimSpace(v.Command) == "" && strings.TrimSpace(v.Manual) == "" {
			problems = append(problems, fmt.Sprintf("verification[%d].command or manual is required", i))
		}
		if strings.TrimSpace(v.Expected) == "" {
			problems = append(problems, fmt.Sprintf("verification[%d].expected is required", i))
		}
	}
	if len(nonEmptyStrings(contract.AcceptanceCriteria)) == 0 {
		problems = append(problems, "acceptance_criteria must include at least one criterion")
	}
	for _, criterion := range extractAcceptanceCriteria(taskBody) {
		if !criterionCovered(criterion, contract.AcceptanceCriteria) {
			problems = append(problems, "acceptance_criteria missing source criterion "+quoteProblem(criterion))
		}
	}
	if strings.TrimSpace(contract.RiskTier) == "" {
		problems = append(problems, "risk_tier is required")
	}
	if strings.TrimSpace(contract.PermissionTier) == "" {
		problems = append(problems, "permission_tier is required")
	}
	if strings.TrimSpace(contract.Rollback) == "" {
		problems = append(problems, "rollback is required")
	}
	for _, id := range collectForeignTaskIDs(raw, taskID) {
		problems = append(problems, "references foreign task ID "+id)
	}
	problems = append(problems, validateSchemaVersion(contract)...)
	sort.Strings(problems)
	return problems
}

// validateSchemaVersion applies the admission-facts rules gated by
// PlanContract.SchemaVersion. Absent and "1" are the original schema — no
// additional fields are required, so pre-existing contracts (and any
// generated without the extension) keep validating exactly as before this
// migration. PlanContractSchemaV2 requires Objective and validates
// RequiredCapabilities against the closed planContractCapabilities registry.
// Any other value is rejected outright rather than silently treated as v1,
// so a typo'd or future schema_version cannot skip the extension's checks.
func validateSchemaVersion(contract PlanContract) []string {
	switch contract.SchemaVersion {
	case "", "1":
		return nil
	case PlanContractSchemaV2:
		var problems []string
		if strings.TrimSpace(contract.Objective) == "" {
			problems = append(problems, "objective is required for schema_version \"2\"")
		}
		problems = append(problems, validateRequiredCapabilities(contract.RequiredCapabilities)...)
		return problems
	default:
		return []string{fmt.Sprintf("unsupported schema_version %q", contract.SchemaVersion)}
	}
}

// validateRequiredCapabilities checks each declared capability against the
// closed planContractCapabilities registry. An unrecognized name is a
// validation problem, never a silent pass — see planContractCapabilities.
func validateRequiredCapabilities(capabilities []string) []string {
	var problems []string
	for _, c := range capabilities {
		name := strings.TrimSpace(c)
		if !planContractCapabilities[name] {
			problems = append(problems, fmt.Sprintf("required_capabilities: unknown capability %q", c))
		}
	}
	return problems
}

// PlanContractPromptJSON re-emits only validated core plan-contract fields for
// model prompts, dropping supplemental fields that may contain agent-authored
// instructions.
func PlanContractPromptJSON(raw, taskID string) (string, error) {
	if problems := ValidatePlanContract(raw, taskID); len(problems) > 0 {
		return "", fmt.Errorf("invalid plan contract: %s", strings.Join(problems, "; "))
	}
	contract, err := parsePlanContract(raw)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(contract); err != nil {
		return "", fmt.Errorf("marshal plan contract: %w", err)
	}
	return strings.TrimSpace(buf.String()), nil
}

func parsePlanContract(raw string) (PlanContract, error) {
	var contract PlanContract
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&contract); err != nil {
		return PlanContract{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return PlanContract{}, fmt.Errorf("multiple top-level values")
	}
	return contract, nil
}

func extractAcceptanceCriteria(body string) []string {
	var criteria []string
	inSection := false
	var current strings.Builder
	flush := func() {
		text := strings.TrimSpace(current.String())
		if text != "" {
			criteria = append(criteria, text)
		}
		current.Reset()
	}
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if isMarkdownHeading(trimmed) {
			flush()
			inSection = isAcceptanceCriteriaHeading(trimmed)
			continue
		}
		if !inSection {
			continue
		}
		if item, ok := listItemText(trimmed); ok {
			flush()
			current.WriteString(item)
			continue
		}
		if current.Len() > 0 && trimmed != "" && isIndentedMarkdownContinuation(line) {
			current.WriteByte(' ')
			current.WriteString(trimmed)
			continue
		}
		if trimmed != "" {
			flush()
		}
	}
	flush()
	return criteria
}

func isIndentedMarkdownContinuation(line string) bool {
	return strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")
}

func isMarkdownHeading(line string) bool {
	if !strings.HasPrefix(line, "#") {
		return false
	}
	line = strings.TrimLeft(line, "#")
	return strings.HasPrefix(line, " ")
}

func isAcceptanceCriteriaHeading(line string) bool {
	line = strings.TrimLeft(line, "#")
	line = strings.TrimSpace(strings.TrimRight(line, "#"))
	return strings.EqualFold(line, "acceptance criteria")
}

func listItemText(line string) (string, bool) {
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if text, ok := strings.CutPrefix(line, prefix); ok {
			return normalizeListItemText(text), true
		}
	}
	for i, r := range line {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' && i > 0 && len(line) > i+1 && line[i+1] == ' ' {
			return normalizeListItemText(line[i+2:]), true
		}
		break
	}
	return "", false
}

func normalizeListItemText(text string) string {
	text = strings.TrimSpace(text)
	for _, marker := range []string{"[ ]", "[x]", "[X]"} {
		if rest, ok := strings.CutPrefix(text, marker); ok {
			return strings.TrimSpace(rest)
		}
	}
	return text
}

func criterionCovered(source string, contractCriteria []string) bool {
	source = normalizeCriterionText(source)
	if source == "" {
		return true
	}
	for _, criterion := range contractCriteria {
		if normalizeCriterionText(criterion) == source {
			return true
		}
	}
	return false
}

func normalizeCriterionText(s string) string {
	if item, ok := listItemText(strings.TrimSpace(s)); ok {
		return normalizeCriterionWhitespace(item)
	}
	return normalizeCriterionWhitespace(normalizeListItemText(s))
}

// normalizeCriterionWhitespace collapses whitespace and strips markdown
// inline-code backticks so a plan agent transcribing a criterion in plain
// prose (e.g. /skill instead of `/skill`) still matches its source.
func normalizeCriterionWhitespace(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func quoteProblem(s string) string {
	s = textutil.TruncateBytesTotal(strings.TrimSpace(s), 140, "...")
	return fmt.Sprintf("%q", s)
}

func validateContractPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(p, '\x00') {
		return fmt.Errorf("contains NUL byte")
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("must use forward slashes")
	}
	if isWindowsDrivePath(p) {
		return fmt.Errorf("must be repository-relative")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return fmt.Errorf("must be repository-relative")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must stay inside the repository")
	}
	if clean != p {
		return fmt.Errorf("must be clean path %q", clean)
	}
	return nil
}

func normalizeExpectedDeletionEntry(raw string) (entry string, isGlob bool, err error) {
	p := strings.TrimSpace(raw)
	if err := validateContractPath(p); err != nil {
		return "", false, err
	}
	if !looksLikeExpectedDeletionGlob(p) {
		return p, false, nil
	}
	if strings.Contains(p, "**") {
		return "", false, fmt.Errorf("recursive ** globs are not supported")
	}
	if _, err := path.Match(p, p); err != nil {
		return "", false, fmt.Errorf("invalid glob pattern: %w", err)
	}
	return p, true, nil
}

func looksLikeExpectedDeletionGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func isWindowsDrivePath(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func nonEmptyStrings(values []string) []string {
	out := values[:0]
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
