package workflow

import (
	"bytes"
	"fmt"
	"text/template"
)

// TemplateContext provides data available in prompt templates and shell commands.
type TemplateContext struct {
	Task    any // task.Task (avoid import cycle — passed as any)
	Step    Step
	Prev    *StepRecord
	Vars    map[string]string
	Project any // *project.Project or nil
}

// RenderTemplate renders a Go text/template string with the given context.
func RenderTemplate(tmpl string, ctx TemplateContext) (string, error) {
	t, err := template.New("step").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}
