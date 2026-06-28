package experience

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func FromTask(t task.Task, proj project.Project) Record {
	return Record{
		TaskID:         t.ID,
		CreatedAt:      recordCreatedAt(t),
		ProjectID:      proj.ID,
		ProjectType:    string(proj.Type),
		Title:          t.Title,
		Tags:           append([]string(nil), t.Tags...),
		Size:           taskSize(t),
		Type:           string(t.TaskType),
		AgentMode:      t.AgentMode,
		Provider:       recordProvider(t),
		Outcome:        t.Outcome,
		Attempts:       len(t.AgentRuns),
		FailureModes:   failureModes(t),
		Strategy:       firstNonEmpty(t.PlanBrief, t.Plan, t.Body),
		VerifyCommands: verifyCommands(proj),
		Caution:        caution(t),
	}
}

func FormatForPrompt(records []Record) string {
	if len(records) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Verified Experience Memory\n")
	b.WriteString("Advisory only: use these prior verified outcomes as context, not as a substitute for checking the current task.\n")
	for i := range records {
		rec := &records[i]
		fmt.Fprintf(&b, "\n- Task %s (%s, %s): %s\n", rec.TaskID, rec.ProjectType, rec.Outcome, rec.Title)
		if len(rec.Tags) > 0 {
			fmt.Fprintf(&b, "  Tags: %s\n", strings.Join(rec.Tags, ", "))
		}
		if rec.Strategy != "" {
			fmt.Fprintf(&b, "  Strategy: %s\n", singleLine(rec.Strategy))
		}
		if len(rec.VerifyCommands) > 0 {
			fmt.Fprintf(&b, "  Verified by: %s\n", strings.Join(rec.VerifyCommands, " && "))
		}
		if len(rec.FailureModes) > 0 {
			fmt.Fprintf(&b, "  Failure modes recovered: %s\n", strings.Join(rec.FailureModes, "; "))
		}
		if rec.Caution != "" {
			fmt.Fprintf(&b, "  Caution: %s\n", singleLine(rec.Caution))
		}
	}
	return b.String()
}

func recordCreatedAt(t task.Task) time.Time {
	if t.ClosedAt != nil && !t.ClosedAt.IsZero() {
		return t.ClosedAt.UTC()
	}
	if !t.UpdatedAt.IsZero() {
		return t.UpdatedAt.UTC()
	}
	return t.CreatedAt.UTC()
}

func recordProvider(t task.Task) string {
	for i := range slices.Backward(t.AgentRuns) {
		if t.AgentRuns[i].Provider != "" {
			return t.AgentRuns[i].Provider
		}
	}
	return t.HandoffSourceProvider
}

func failureModes(t task.Task) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for i := range t.AgentRuns {
		run := &t.AgentRuns[i]
		add(run.ProtocolViolation)
		add(run.TestOutcome)
		if run.TestFailureFingerprint != "" {
			add("test_failure:" + run.TestFailureFingerprint)
		}
	}
	return out
}

func verifyCommands(proj project.Project) []string {
	if proj.Checks == nil {
		return nil
	}
	return append([]string(nil), proj.Checks.Verify...)
}

func caution(t task.Task) string {
	return firstNonEmpty(t.StatusReason, t.PlanCritique, t.CodeReview)
}

func taskSize(t task.Task) string {
	switch {
	case t.PRNumber > 0:
		return "pr"
	case len(t.Body) > 1200 || len(t.Plan) > 1200:
		return "large"
	case len(t.Body) > 0 || len(t.Plan) > 0:
		return "small"
	default:
		return "unknown"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func singleLine(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}
