package workflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func implementedExec() *Execution {
	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	now := time.Now().UTC()
	wf.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})
	return wf
}

func makeLintVerifyRepo(t *testing.T) string {
	t.Helper()
	wt := makeBaseRepo(t, map[string]string{
		"go.mod":                   "module example.com/verifyrepo\n\ngo 1.26.5\n",
		"internal/bar/bar.go":      "package bar\n\nfunc Value() int { return 1 }\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Value() (int, error) { return 1, nil }\n",
		"internal/foo/foo_test.go": "package foo\n",
		"internal/keep/keep.go":    "package keep\n\nfunc Value() int { return 1 }\n",
	})
	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Value() (int, error) { return 2, nil }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: change foo")
	return wt
}

func makeFrontendVerifyRepo(t *testing.T) string {
	t.Helper()
	wt := makeBaseRepo(t, map[string]string{
		"frontend/src/pages/Settings.svelte":         "<script lang=\"ts\">export let loaded = false</script>\n",
		"frontend/src/pages/Settings.svelte.test.ts": "import { describe, it } from 'vitest'\n\ndescribe('Settings', () => { it('loads', () => {}) })\n",
		"frontend/src/pages/Other.svelte":            "<p>unchanged</p>\n",
	})
	writeRepoFile(t, wt, "frontend/src/pages/Settings.svelte", "<script lang=\"ts\">import { GetPathExplanations } from '$lib/api'\nexport let loaded = !!GetPathExplanations\n</script>\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: change settings")
	return wt
}

func lintVerifyCommand(file string) string {
	return "grep -q 'func Value() (v int, err error)' " + file +
		" || { printf '" + file + ":3:1: unnamedResult: consider giving a name to these results (gocritic)\\n' >&2; exit 1; } # golangci-lint"
}

func lintVerifyOutput(files ...string) string {
	var b strings.Builder
	for _, file := range files {
		b.WriteString(file)
		b.WriteString(":3:1: unnamedResult: consider giving a name to these results (gocritic)\n")
	}
	return b.String()
}

func lintVerifyCommandForFiles(files ...string) string {
	return "printf '" + strings.ReplaceAll(lintVerifyOutput(files...), "\n", "\\n") + "' >&2; exit 1 # golangci-lint"
}

func frontendVerifyCommand() string {
	return "(cd frontend && mise exec -- npm run test:coverage)"
}

func frontendVerifyOutput() string {
	return " RUN  v3.2.4 /tmp/verify/frontend\n\n" +
		"stderr | src/pages/Settings.svelte.test.ts > Settings > loads path explanations\n" +
		"Error: [vitest] No \"GetPathExplanations\" export is defined on the \"$lib/api\" mock. Did you forget to return it from \"vi.mock\"?\n" +
		"If you need to partially mock a module, you can use \"importOriginal\" helper inside:\n" +
		"frontend/src/pages/Settings.svelte:150:8\n"
}

func frontendVerifyOutputForPath(path string) string {
	return " RUN  v3.2.4 /tmp/verify/frontend\n\n" +
		"stderr | " + path + " > Settings > loads path explanations\n" +
		"Error: [vitest] No \"GetPathExplanations\" export is defined on the \"$lib/api\" mock. Did you forget to return it from \"vi.mock\"?\n"
}

func writeFrontendVerifyMise(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"exec\" ] && [ \"$2\" = \"--\" ] && [ \"$3\" = \"npm\" ] && [ \"$4\" = \"run\" ] && [ \"$5\" = \"test:coverage\" ]; then\n" +
		"  cat <<'EOF' >&2\n" + frontendVerifyOutput() + "EOF\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo \"unexpected mise invocation: $*\" >&2\n" +
		"exit 2\n"
	path := filepath.Join(binDir, "mise")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	return binDir
}

func TestExecVerifyChecks_AutoFixRewindsToImplement(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{lintVerifyCommand("internal/foo/foo.go")})
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "impl-1", Role: "implementation"}},
	})

	wf := implementedExec()
	rec := wf.RecordForStep(verifyChecksImplStepID)
	if rec == nil {
		t.Fatal("implement step record missing")
		panic("unreachable")
	}
	rec.AgentID = "impl-1"
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "impl-1", Role: "implementation"}},
	})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.StepID != "" || out.Status != "" {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Errorf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want %q", wf.State, ExecWaiting)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Errorf("auto_fix counter = %q, want 1", wf.Variables["step.verify_checks.auto_fix"])
	}
	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "FAILED the project verify suite") ||
		!strings.Contains(note, "internal/foo/foo.go:3:1: unnamedResult") ||
		!strings.Contains(note, "golangci-lint") {
		t.Errorf("reask note missing failure context:\n%s", note)
	}
	if wf.Variables[workflowRetryAfterVar] == "" {
		t.Errorf("retry-after not set")
	}
	if got := wf.Variables[verifyAutoFixRunIDVar]; got != "impl-1" {
		t.Errorf("rewound run agent id = %q, want impl-1", got)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress (not escalated on first failure)", ti.Status)
	}
}

func TestExecVerifyChecks_AutoFixReasksBelowCeiling(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{lintVerifyCommand("internal/foo/foo.go")})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	// A code-fixable lint failure below the generic ceiling keeps being
	// re-asked rather than escalating.
	wf := implementedExec()
	wf.Variables["step.verify_checks.auto_fix"] = "2"
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked (keep re-asking, never escalate)", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID || wf.State != ExecWaiting {
		t.Fatalf("workflow after rewind = %+v, want implement/ExecWaiting", wf)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "3" {
		t.Errorf("auto_fix counter = %q, want 3", got)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress (never escalated to human)", ti.Status)
	}
}

func TestExecVerifyChecks_AutoFixIdenticalFingerprintEscalatesEarly(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	cmd := lintVerifyCommand("internal/foo/foo.go")
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("first attempt err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("first parked output should be zero, got %+v", out)
	}
	now := time.Now().UTC()
	wf.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})

	out, err = engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "repeated identical auto-fix failures") {
		t.Fatalf("reason = %q, want identical-failure exhaustion", ti.StatusReason)
	}
}

func TestAutoFixFailureFingerprintFallsBackToOutputTail(t *testing.T) {
	t.Parallel()
	const cmd = "mise exec -- go test ./internal/workflow"

	first := autoFixFailureFingerprint(cmd, "$ "+cmd+"\n--- FAIL: TestAlpha\n    alpha_test.go:12: got false\nFAIL\n")
	second := autoFixFailureFingerprint(cmd, "$ "+cmd+"\n--- FAIL: TestBeta\n    beta_test.go:34: got 0\nFAIL\n")
	if first == second {
		t.Fatal("fingerprints matched for distinct non-frontend failures under the same command")
	}
}

func TestExecVerifyChecks_AutoFixCeilingEscalates(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{lintVerifyCommand("internal/foo/foo.go")})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	// A deterministic failure that survives the ceiling finally escalates so a
	// human is paged instead of the loop running forever.
	wf := implementedExec()
	wf.Variables["step.verify_checks.auto_fix"] = strconv.Itoa(verifyChecksAutoFixCeiling)
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged", out.Output)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required at ceiling", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "escalating after") {
		t.Errorf("reason = %q, want exhaustion note", ti.StatusReason)
	}
}

func TestExecVerifyChecks_AutoFixMultipleChangedLintFiles(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	writeRepoFile(t, wt, "internal/bar/bar.go", "package bar\n\nfunc Value() (int, error) { return 2, nil }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "--amend", "--no-edit")

	engine, tasks := newVerifyChecksEngine(t, wt, []string{
		lintVerifyCommandForFiles("internal/foo/foo.go", "internal/bar/bar.go"),
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID || wf.State != ExecWaiting {
		t.Fatalf("workflow after rewind = %+v, want implement/ExecWaiting", wf)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "1" {
		t.Fatalf("auto_fix counter = %q, want 1", got)
	}
	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "internal/foo/foo.go:3:1: unnamedResult") ||
		!strings.Contains(note, "internal/bar/bar.go:3:1: unnamedResult") {
		t.Fatalf("reask note missing multi-file lint detail:\n%s", note)
	}
}

// incidentLinterVerifyOutput mimics real golangci-lint output combining three
// distinct linters (errorlint, exhaustive, typecheck) on the same changed
// file — regression coverage for the incident where these linters' findings
// were not recognized as auto-fixable because they were assumed to be
// gocritic-shaped.
func incidentLinterVerifyOutput(file string) string {
	return file + ":10:5: comparing with == will fail on wrapped errors, use errors.Is (errorlint)\n" +
		file + ":20:2: missing cases in switch of type Kind: KindBar (exhaustive)\n" +
		file + ":30:1: undefined: someIdentifier (typecheck)\n"
}

// incidentLinterVerifyCommand runs the fixture through a script FILE rather
// than inlining the finding text into the shell command string itself. Every
// invocation is echoed verbatim ("$ <raw command>\n") into the same output
// buffer parseGolangCILintGoFiles scans (engine_steps_verify_checks.go's
// runVerifyCommands); a command that embeds "<file>:<line>:<col>:" literally
// (e.g. an inline `printf '...file.go:10:5...'`) makes that echoed line match
// verifyGolangCILintFindingRe on its own, well before the real output — which
// silently corrupts the classifier's lintFiles with the whole command string
// as a bogus "file" that changedFiles never contains, forcing
// classifyCodeFixableLintFailure's ok=false path. Only the script *path* gets
// echoed here, so the actual finding lines are the only ones matched.
func incidentLinterVerifyCommand(t *testing.T, file string) string {
	t.Helper()
	script := "#!/bin/sh\ncat <<'EOF' >&2\n" + incidentLinterVerifyOutput(file) + "EOF\nexit 1\n"
	path := filepath.Join(t.TempDir(), "incident-lint.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	return "sh " + path + " # golangci-lint"
}

func TestExecVerifyChecks_AutoFixIncidentLinters(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	engine, tasks := newVerifyChecksEngine(t, wt, []string{incidentLinterVerifyCommand(t, "internal/foo/foo.go")})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Errorf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want %q", wf.State, ExecWaiting)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "1" {
		t.Errorf("auto_fix counter = %q, want 1", got)
	}

	// The rewind/counter/note assertions above are also produced by the
	// unclassified catch-all fallback in execVerifyChecks (it calls the same
	// autoFixOrFlagVerifyChecks with a generic reason), so they alone cannot
	// tell "classifier correctly recognized errorlint/exhaustive/typecheck as
	// code_fixable_lint" apart from "classifier recognized nothing and the
	// fallback happened to produce the same shape". The recorded
	// verify-checks.json artifact is the one place the classifier's actual
	// verdict is observable, so assert on it directly.
	var report verifyChecksReport
	found := false
	for _, put := range rec.puts {
		if put.name != "verify-checks.json" {
			continue
		}
		found = true
		if err := json.Unmarshal([]byte(put.content), &report); err != nil {
			t.Fatalf("unmarshal verify-checks.json: %v", err)
			panic("unreachable")
		}
	}
	if !found {
		t.Fatalf("no verify-checks.json artifact recorded")
	}
	if report.Classification != "code_fixable_lint" {
		t.Fatalf("classification = %q, want code_fixable_lint (errorlint/exhaustive/typecheck findings not recognized as auto-fixable lint)", report.Classification)
	}
	if !slices.Contains(report.ChangedFiles, "internal/foo/foo.go") {
		t.Fatalf("classified ChangedFiles = %v, want to contain internal/foo/foo.go", report.ChangedFiles)
	}

	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "internal/foo/foo.go:10:5") ||
		!strings.Contains(note, "errorlint") ||
		!strings.Contains(note, "internal/foo/foo.go:20:2") ||
		!strings.Contains(note, "exhaustive") ||
		!strings.Contains(note, "internal/foo/foo.go:30:1") ||
		!strings.Contains(note, "typecheck") ||
		!strings.Contains(note, "golangci-lint") {
		t.Errorf("reask note missing incident-linter finding detail:\n%s", note)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress (not escalated on first failure)", ti.Status)
	}
}

func TestExecVerifyChecks_DeterministicFrontendFailureRewindsToImplement(t *testing.T) {
	t.Parallel()
	wt := makeFrontendVerifyRepo(t)
	binDir := writeFrontendVerifyMise(t)
	cmd := `PATH="` + filepath.ToSlash(binDir) + `:$PATH"; export PATH; ` + frontendVerifyCommand()
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.StepID != "" || out.Status != "" {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Fatalf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", wf.State, ExecWaiting)
	}
	if got := wf.Variables["step.verify_checks.auto_fix"]; got != "1" {
		t.Fatalf("auto_fix counter = %q, want 1", got)
	}
	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "npm run test:coverage") {
		t.Fatalf("verify reask note missing command:\n%s", note)
	}
	if !strings.Contains(note, "Highest-signal failure excerpt") {
		t.Fatalf("verify reask note missing highest-signal section:\n%s", note)
	}
	if !strings.Contains(note, `No "GetPathExplanations" export is defined on the "$lib/api" mock`) {
		t.Fatalf("verify reask note missing vitest failure excerpt:\n%s", note)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", ti.Status)
	}
}

func TestExecVerifyChecks_DeterministicFrontendFailureReasksBelowCeiling(t *testing.T) {
	t.Parallel()
	wt := makeFrontendVerifyRepo(t)
	binDir := writeFrontendVerifyMise(t)
	cmd := `PATH="` + filepath.ToSlash(binDir) + `:$PATH"; export PATH; ` + frontendVerifyCommand()
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	wf.Variables["step.verify_checks.auto_fix"] = "2"
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked (keep re-asking, never escalate)", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID || wf.State != ExecWaiting {
		t.Fatalf("workflow after rewind = %+v, want implement/ExecWaiting", wf)
	}
	ti := mustGetTaskInfo(t, tasks, "t1")
	if ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress (never escalated to human)", ti.Status)
	}
}

func TestClassifyDeterministicFrontendVerifyFailureMatchesFrontendCwdPath(t *testing.T) {
	t.Parallel()
	wt := makeFrontendVerifyRepo(t)

	excerpt, changedFiles, ok := classifyDeterministicFrontendVerifyFailure(
		t.Context(), "t1", wt, frontendVerifyCommand(),
		frontendVerifyOutputForPath("src/pages/Settings.svelte:150:8"),
		"",
	)
	if !ok {
		t.Fatalf("ok = false, want true; changedFiles=%v excerpt=%q", changedFiles, excerpt)
	}
	if !strings.Contains(excerpt, "GetPathExplanations") {
		t.Fatalf("excerpt = %q, want frontend failure detail", excerpt)
	}
}

func TestClassifyDeterministicFrontendVerifyFailureIgnoresUnchangedFrontendCwdPath(t *testing.T) {
	t.Parallel()
	wt := makeFrontendVerifyRepo(t)

	_, changedFiles, ok := classifyDeterministicFrontendVerifyFailure(
		t.Context(), "t1", wt, frontendVerifyCommand(),
		frontendVerifyOutputForPath("src/routes/Login.test.ts:42:1"),
		"",
	)
	if ok {
		t.Fatalf("ok = true for unchanged frontend-cwd citation; changedFiles=%v", changedFiles)
	}
}

func TestExecVerifyChecks_LintRepairAdvancesToReadyReview(t *testing.T) {
	t.Parallel()
	wt := makeLintVerifyRepo(t)
	store := newInlineTestStore(t, "verify-lint-repair", `
id: verify-lint-repair
name: Verify lint repair
steps:
  - id: set_in_progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: implement
  - id: implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      model: sonnet
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: verify_checks
  - id: verify_checks
    type: verify_checks
    next:
      - when:
          field: task.status
          operator: equals
          value: blocked
        goto: ""
      - when:
          field: task.status
          operator: equals
          value: human-required
        goto: ""
      - goto: set_ready_review
  - id: set_ready_review
    type: set_status
    config:
      status: ready-review
    next:
      - goto: ""
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.setCheckConfigGetterForTest(&fakeCheckGetter{cmds: []string{lintVerifyCommand("internal/foo/foo.go")}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "verify-lint-repair"); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got := agents.LastCall().Role; got != "implementation" {
		t.Fatalf("first agent role = %q, want implementation", got)
	}

	firstAgentID := agents.LastID()
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: firstAgentID, Success: true, Result: "first pass"})

	first := mustGetTaskInfo(t, tasks, "t1")
	if first.Status != "in-progress" {
		t.Fatalf("status after lint reask = %q, want in-progress", first.Status)
	}
	if first.Workflow == nil || first.Workflow.CurrentStep != verifyChecksImplStepID || first.Workflow.State != ExecWaiting {
		t.Fatalf("workflow after lint reask = %+v, want implement/ExecWaiting", first.Workflow)
		panic("unreachable")
	}
	if first.Workflow.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Fatalf("auto_fix counter after first failure = %q, want 1", first.Workflow.Variables["step.verify_checks.auto_fix"])
	}
	if !strings.Contains(first.Workflow.Variables[verifyReaskNoteVar], "unnamedResult") {
		t.Fatalf("verify_reask_note missing lint detail:\n%s", first.Workflow.Variables[verifyReaskNoteVar])
	}

	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Value() (v int, err error) { return 2, nil }\n")

	first.Workflow.SetVar(workflowRetryAfterVar, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	if err := tasks.SetWorkflow("t1", first.Workflow); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	engine.ResumeStalled()
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls after resume = %d, want 2", got)
	}
	secondAgentID := agents.LastID()
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: secondAgentID, Success: true, Result: "repaired"})

	final := mustGetTaskInfo(t, tasks, "t1")
	if final.Status != "ready-review" {
		t.Fatalf("final status = %q, want ready-review", final.Status)
	}
	if final.Workflow == nil || final.Workflow.State != ExecCompleted || final.Workflow.CurrentStep != "" {
		t.Fatalf("final workflow = %+v, want completed terminal workflow", final.Workflow)
		panic("unreachable")
	}
	if strings.Contains(final.StatusReason, "human-required") {
		t.Fatalf("final status reason unexpectedly escalated: %q", final.StatusReason)
	}
}

func TestExecVerifyChecks_NoImplementStepEscalates(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged (no implement step to rewind to)", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}
