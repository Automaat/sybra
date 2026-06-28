package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

type PlanContract struct {
	TaskID             string                `json:"task_id"`
	Branch             string                `json:"branch"`
	Worktree           string                `json:"worktree"`
	Files              []PlanContractFile    `json:"files"`
	Steps              []string              `json:"steps"`
	Verification       []PlanContractCommand `json:"verification"`
	AcceptanceCriteria []string              `json:"acceptance_criteria"`
	RiskTier           string                `json:"risk_tier"`
	PermissionTier     string                `json:"permission_tier"`
	Rollback           string                `json:"rollback"`
}

type PlanContractFile struct {
	Path    string   `json:"path"`
	Purpose string   `json:"purpose,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
}

type PlanContractCommand struct {
	Command  string `json:"command"`
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
	var contract PlanContract
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&contract); err != nil {
		return []string{"malformed JSON: " + err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return []string{"malformed JSON: multiple top-level values"}
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
	if len(contract.Verification) == 0 {
		problems = append(problems, "verification must include at least one command")
	}
	for i, v := range contract.Verification {
		if strings.TrimSpace(v.Command) == "" {
			problems = append(problems, fmt.Sprintf("verification[%d].command is required", i))
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
	sort.Strings(problems)
	return problems
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
		if current.Len() > 0 && trimmed != "" {
			current.WriteByte(' ')
			current.WriteString(trimmed)
		}
	}
	flush()
	return criteria
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
			return strings.TrimSpace(text), true
		}
	}
	for i, r := range line {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' && i > 0 && len(line) > i+1 && line[i+1] == ' ' {
			return strings.TrimSpace(line[i+2:]), true
		}
		break
	}
	return "", false
}

func criterionCovered(source string, contractCriteria []string) bool {
	source = normalizeCriterionWhitespace(source)
	if source == "" {
		return true
	}
	for _, criterion := range contractCriteria {
		if normalizeCriterionWhitespace(criterion) == source {
			return true
		}
	}
	return false
}

func normalizeCriterionWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func quoteProblem(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 140 {
		s = s[:137] + "..."
	}
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
