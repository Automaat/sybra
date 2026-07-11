package workflow

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// TemplateContext provides data available in prompt templates and shell commands.
type TemplateContext struct {
	Task     TaskInfo
	Step     Step
	Prev     *StepRecord
	Vars     map[string]string
	Project  any        // *project.Project or nil
	Workflow *Execution // current execution snapshot; nil outside workflow context
}

// RenderTemplate renders a Go text/template string with the given context.
func RenderTemplate(tmpl string, ctx TemplateContext) (string, error) {
	t, err := template.New("step").Funcs(templateFuncs).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

var templateFuncs = template.FuncMap{
	"shellquote":          shellQuote,
	"getvar":              getVar,
	"recoveredorprev":     recoveredOrPrev,
	"plancontractjson":    PlanContractPromptJSON,
	"currenttestfailures": currentTestFailures,
}

// currentTestFailures returns the current "## Test Failures" section from a
// task body, trimmed, or "" if none is present. Inlined directly into the
// reimplementation prompt (rather than pointing the agent at a CLI fetch it
// must parse correctly) so the current failure survives an agent truncating
// or misreading its own `sybra-cli get` output. At most one such section is
// ever live in a body (see stripTestFailuresSections), so the first match is
// unambiguously current.
func currentTestFailures(body string) string {
	return strings.TrimSpace(testFailSectionOf(body))
}

// getVar safely retrieves a variable from a map, returning "" if absent.
func getVar(vars map[string]string, key string) string {
	return vars[key]
}

// shellQuote wraps a string in single quotes with proper escaping for bash.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// recoveredOrPrev returns empty when the execution was recovered from a stale
// interactive session (no real agent output exists), otherwise returns the
// previous step's output. Use in workflow prompts instead of .Prev.Output to
// guard against stale content after a session recovery.
func recoveredOrPrev(wf *Execution, prev *StepRecord) string {
	if wf != nil && wf.Recovered {
		return ""
	}
	if prev == nil {
		return ""
	}
	return prev.Output
}
