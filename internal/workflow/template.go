package workflow

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
	"text/template"

	"github.com/Automaat/sybra/internal/textutil"
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
	"commitsignflags":     commitSignFlagsVar,
	"sidecardir":          sidecarDirVar,
	"recoveredorprev":     recoveredOrPrev,
	"plancontractjson":    PlanContractPromptJSON,
	"currenttestfailures": currentTestFailures,
	"acceptanceledger":    acceptanceLedger,
}

// sidecarDirVar returns the directory workflow scratch output belongs in: the
// writable per-task sandbox dir when one is set, otherwise the worktree.
//
// Verifier roles run against a read-only worktree so they cannot alter the
// code they judge (#2791), which means their own output cannot live there.
// The fallback keeps every flow working when no resolver is wired — the
// pre-#2791 behaviour — so this is safe to use unconditionally in templates.
func sidecarDirVar(vars map[string]string) string {
	if v := strings.TrimSpace(vars[WorkflowVarSidecarDir]); v != "" {
		return v
	}
	return vars[WorkflowVarDir]
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
	if content == "" {
		return ""
	}
	if section, ok := topLevelSection(content, isTestFailuresHeading); ok {
		return clampPromptInline(section)
	}
	return ""
}

func acceptanceLedger(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if section, ok := topLevelSection(content, func(line string) bool {
		return strings.EqualFold(line, acceptanceLedgerHeading)
	}); ok {
		return clampPromptInline(section)
	}
	return ""
}

func topLevelSection(body string, match func(string) bool) (string, bool) {
	start, end, ok := topLevelSectionRange(body, match)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(body[start:end]), true
}

func topLevelSectionRange(body string, match func(string) bool) (start, end int, ok bool) {
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
		if inFence || !match(trimmed) {
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

// WorkflowVarCommitSignFlags names the variable a dispatcher seeds with the
// host's resolved commit flags.
const WorkflowVarCommitSignFlags = "commit_sign_flags"

// defaultCommitSignFlags backs commitSignFlagsVar for the workflows no
// dispatcher seeds. Package-level rather than an Engine field because
// templates render through the free RenderTemplate from seven call sites, none
// of which carry an Engine; the value is a single process-wide deployment
// posture, so there is nothing per-execution to thread.
var defaultCommitSignFlags atomic.Value

// SetDefaultCommitSignFlags installs the fallback commit flags for prompts
// whose workflow never seeds WorkflowVarCommitSignFlags. Wired from config at
// startup. Unset keeps "-s".
func SetDefaultCommitSignFlags(flags string) {
	if flags = strings.TrimSpace(flags); flags != "" {
		defaultCommitSignFlags.Store(flags)
	}
}

// commitSignFlagsVar returns the git commit flags a prompt should instruct an
// agent to use.
//
// Prefer this over a bare getvar: only the pr-fix dispatcher seeds the
// variable, so getvar renders an empty string — and therefore a broken
// `git commit ` — in every other workflow. The final "-s" fallback also fails
// in the safe direction, since an unsignable `-S` hard-fails the commit and
// parks the task, while a missing one costs only a signature no gate requires.
// DCO sign-off is enforced independently by the prepare-commit-msg hook.
func commitSignFlagsVar(vars map[string]string) string {
	if v := strings.TrimSpace(vars[WorkflowVarCommitSignFlags]); v != "" {
		return v
	}
	if v, ok := defaultCommitSignFlags.Load().(string); ok && v != "" {
		return v
	}
	return "-s"
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
	head := textutil.TruncateBytes(body, promptInlineHeadBytes, "")
	tail := textutil.TailBytes(body, promptInlineMaxBytes-promptInlineHeadBytes)
	return head + promptInlineElision + tail
}

// DefaultCommitSignFlags reports the configured fallback. Exported for the
// app-layer hot-reload test, which has no other way to observe that a reload
// reached this sink.
func DefaultCommitSignFlags() string {
	return commitSignFlagsVar(nil)
}
