package workflow

import (
	"strings"
	"testing"
)

func TestRenderTemplate_Basic(t *testing.T) {
	t.Parallel()
	ctx := TemplateContext{Task: TaskInfo{Title: "My Task"}}
	got, err := RenderTemplate("{{.Task.Title}}", ctx)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if got != "My Task" {
		t.Errorf("got %q, want %q", got, "My Task")
	}
}

func TestRenderTemplate_ShellQuote(t *testing.T) {
	t.Parallel()
	ctx := TemplateContext{Task: TaskInfo{Title: "it's done"}}
	got, err := RenderTemplate("{{shellquote .Task.Title}}", ctx)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	want := "'it'\"'\"'s done'"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderTemplate_GetVar_Present(t *testing.T) {
	t.Parallel()
	ctx := TemplateContext{Vars: map[string]string{"key": "value"}}
	got, err := RenderTemplate(`{{getvar .Vars "key"}}`, ctx)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if got != "value" {
		t.Errorf("got %q, want %q", got, "value")
	}
}

func TestRenderTemplate_GetVar_Absent(t *testing.T) {
	t.Parallel()
	ctx := TemplateContext{Vars: map[string]string{}}
	got, err := RenderTemplate(`{{getvar .Vars "missing"}}`, ctx)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestRenderTemplate_MissingKey(t *testing.T) {
	t.Parallel()
	ctx := TemplateContext{Vars: map[string]string{}}
	// Direct map key access with missingkey=error should fail.
	_, err := RenderTemplate("{{.Vars.doesnotexist}}", ctx)
	if err == nil {
		t.Fatal("expected error for missing map key")
	}
}

func TestRenderTemplate_InvalidSyntax(t *testing.T) {
	t.Parallel()
	_, err := RenderTemplate("{{.Unclosed", TemplateContext{})
	if err == nil {
		t.Fatal("expected parse error for invalid syntax")
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's fine", "'it'\"'\"'s fine'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
			// Verify it actually produces valid bash-safe output.
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Errorf("shellQuote result %q must start and end with single quote", got)
			}
		})
	}
}

func TestRecoveredOrPrev(t *testing.T) {
	t.Parallel()
	prev := &StepRecord{Output: "agent output"}
	tests := []struct {
		name string
		wf   *Execution
		prev *StepRecord
		want string
	}{
		{"recovered clears output", &Execution{Recovered: true}, prev, ""},
		{"not recovered returns prev", &Execution{Recovered: false}, prev, "agent output"},
		{"nil prev returns empty", &Execution{Recovered: false}, nil, ""},
		{"nil workflow returns prev", nil, prev, "agent output"},
		{"nil workflow nil prev", nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := recoveredOrPrev(tt.wf, tt.prev)
			if got != tt.want {
				t.Errorf("recoveredOrPrev = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderTemplate_RecoveredOrPrev(t *testing.T) {
	t.Parallel()
	prev := &StepRecord{Output: "real output"}

	t.Run("recovered returns empty", func(t *testing.T) {
		t.Parallel()
		ctx := TemplateContext{
			Workflow: &Execution{Recovered: true},
			Prev:     prev,
		}
		got, err := RenderTemplate("{{recoveredorprev .Workflow .Prev}}", ctx)
		if err != nil {
			t.Fatalf("RenderTemplate: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty on recovery", got)
		}
	})

	t.Run("not recovered returns prev output", func(t *testing.T) {
		t.Parallel()
		ctx := TemplateContext{
			Workflow: &Execution{Recovered: false},
			Prev:     prev,
		}
		got, err := RenderTemplate("{{recoveredorprev .Workflow .Prev}}", ctx)
		if err != nil {
			t.Fatalf("RenderTemplate: %v", err)
		}
		if got != "real output" {
			t.Errorf("got %q, want %q", got, "real output")
		}
	})
}

func TestGetVar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vars map[string]string
		key  string
		want string
	}{
		{"existing key", map[string]string{"k": "v"}, "k", "v"},
		{"missing key", map[string]string{}, "missing", ""},
		{"nil map", nil, "k", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := getVar(tt.vars, tt.key)
			if got != tt.want {
				t.Errorf("getVar(%v, %q) = %q, want %q", tt.vars, tt.key, got, tt.want)
			}
		})
	}
}

func TestAcceptanceLedger(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"## Problem",
		"Keep the workflow converged.",
		"",
		acceptanceLedgerHeading,
		"",
		"### Ledger entry fp-1",
		"",
		ledgerEntryMarker("fp-1"),
		"",
		"> first repro snapshot",
		">",
		"### Ledger entry fp-2",
		"",
		ledgerEntryMarker("fp-2"),
		"",
		"> second repro snapshot",
		">",
		"```text",
		"## Not a heading inside fence",
		"```",
		"",
		"## Notes",
		"Done.",
	}, "\n")
	got := acceptanceLedger(body)
	if !strings.Contains(got, acceptanceLedgerHeading) {
		t.Fatalf("acceptanceLedger = %q, want section heading", got)
	}
	if !strings.Contains(got, "### Ledger entry fp-1") || !strings.Contains(got, "### Ledger entry fp-2") {
		t.Fatalf("acceptanceLedger = %q, want both ledger entries", got)
	}
	if strings.Contains(got, "## Notes") {
		t.Fatalf("acceptanceLedger = %q, must stop before the next top-level heading", got)
	}
}

func TestAcceptanceLedgerPromptRendering(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"## Problem",
		"Retain all distinct product-bug fixes together.",
		"",
		testFailuresHeading,
		"",
		"current failure snapshot",
		"",
		acceptanceLedgerHeading,
		"",
		"### Ledger entry fp-1",
		"",
		ledgerEntryMarker("fp-1"),
		"",
		"first repro",
		"",
		"### Ledger entry fp-2",
		"",
		ledgerEntryMarker("fp-2"),
		"",
		"second repro",
	}, "\n")
	ctx := TemplateContext{
		Task: TaskInfo{ID: "task-ledger", Body: body},
	}

	implementPrompt, err := RenderTemplate(mustBuiltinDefinition(t, "simple-task-implement").StepByID("implement").Config.Prompt, ctx)
	if err != nil {
		t.Fatalf("render implement prompt: %v", err)
	}
	if !strings.Contains(implementPrompt, "## Acceptance Ledger — all must hold simultaneously") {
		t.Fatalf("implement prompt missing ledger framing:\n%s", implementPrompt)
	}
	if !strings.Contains(implementPrompt, "### Ledger entry fp-1") || !strings.Contains(implementPrompt, "### Ledger entry fp-2") {
		t.Fatalf("implement prompt missing ledger entries:\n%s", implementPrompt)
	}

	testPrompt, err := RenderTemplate(mustBuiltinDefinition(t, "testing-task").StepByID("run_test").Config.Prompt, ctx)
	if err != nil {
		t.Fatalf("render testing prompt: %v", err)
	}
	if !strings.Contains(testPrompt, "Reproduce and re-verify EVERY ledger entry against the") {
		t.Fatalf("testing prompt missing re-verify framing:\n%s", testPrompt)
	}
	if !strings.Contains(testPrompt, "### Ledger entry fp-1") || !strings.Contains(testPrompt, "### Ledger entry fp-2") {
		t.Fatalf("testing prompt missing ledger entries:\n%s", testPrompt)
	}
}
