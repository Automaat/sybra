package workflow

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"unicode/utf8"
)

const (
	promptInlineMaxBytes  = 8 * 1024
	promptInlineHeadBytes = promptInlineMaxBytes / 3
	promptInlineElision   = "\n\n…(middle elided to fit prompt)…\n\n"
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
	"acceptanceledger":    acceptanceLedger,
}

// currentTestFailures returns the current "## Test Failures" section from a
// task body, trimmed, or "" if none is present. Inlined directly into the
// reimplementation prompt (rather than pointing the agent at a CLI fetch it
// must parse correctly) so the current failure survives an agent truncating
// or misreading its own `sybra-cli get` output. At most one such section is
// ever live in a body (see stripTestFailuresSections), so the first match is
// unambiguously current.
func currentTestFailures(content string) string {
	content = strings.TrimSpace(content)
	if content == "" || testFailSectionOf(content) == "" {
		return ""
	}
	return clampPromptInline(content)
}

func acceptanceLedger(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if start, end, ok := topLevelSectionRange(content, acceptanceLedgerHeading); ok {
		content = strings.TrimSpace(content[start:end])
	}
	return clampPromptInline(content)
}

func topLevelSectionRange(body, heading string) (start, end int, ok bool) {
	lines := strings.SplitAfter(body, "\n")
	offsets := make([]int, len(lines)+1)
	for i := range lines {
		offsets[i+1] = offsets[i] + len(lines[i])
	}

	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.EqualFold(trimmed, heading) {
			continue
		}

		sectionInFence := false
		end = len(body)
		for j := i + 1; j < len(lines); j++ {
			jTrimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(jTrimmed, "```") || strings.HasPrefix(jTrimmed, "~~~") {
				sectionInFence = !sectionInFence
				continue
			}
			if !sectionInFence && strings.HasPrefix(jTrimmed, "## ") {
				end = offsets[j]
				break
			}
		}
		return offsets[i], end, true
	}
	return 0, 0, false
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

func clampPromptInline(body string) string {
	if len(body) <= promptInlineMaxBytes {
		return body
	}
	head := trimPromptRuneBoundaryEnd(body[:promptInlineHeadBytes])
	tail := trimPromptRuneBoundaryStart(body[len(body)-(promptInlineMaxBytes-promptInlineHeadBytes):])
	return head + promptInlineElision + tail
}

func trimPromptRuneBoundaryStart(s string) string {
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func trimPromptRuneBoundaryEnd(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
