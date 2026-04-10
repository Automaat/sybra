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
	"shellquote":      shellQuote,
	"getvar":          getVar,
	"recoveredorprev": recoveredOrPrev,
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
