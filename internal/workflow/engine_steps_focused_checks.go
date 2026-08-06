package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const focusedChecksReaskNoteVar = "focused_checks_reask_note"

type focusedChecksReport struct {
	ChangedFiles []string               `json:"changedFiles,omitempty"`
	Selected     []focusedCheckArtifact `json:"selected,omitempty"`
	Commands     []string               `json:"commands,omitempty"`
	FailedCmd    string                 `json:"failedCmd,omitempty"`
	OutputTail   string                 `json:"outputTail,omitempty"`
}

type focusedCheckArtifact struct {
	Name         string   `json:"name,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Packages     []string `json:"packages,omitempty"`
	ChangedFiles []string `json:"changedFiles,omitempty"`
}

type selectedFocusedCheck struct {
	FocusedCheck project.FocusedCheck
	ChangedFiles []string
}

func (e *Engine) execFocusedChecks(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if e.checks == nil {
		return stepDone(step, "skipped: no check config getter")
	}
	focused := e.checks.FocusedChecks(e.ctx, taskID)
	if len(focused) == 0 {
		return stepDone(step, "skipped: no focused checks configured")
	}
	if e.worktrees == nil {
		return stepDone(step, "skipped: no worktree getter configured")
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return stepDone(step, "skipped: no worktree for task")
	}

	changedFiles, err := changedFilesSinceProjectBase(e.ctx, wtPath, e.focusedChecksBaseRef(taskID))
	if err != nil {
		return StepOutput{}, fmt.Errorf("focused-checks: discover changed files: %w", err)
	}
	if len(changedFiles) == 0 {
		return stepDone(step, "skipped: no changed files")
	}

	selected, cmds := selectFocusedChecks(focused, changedFiles)
	report := focusedChecksReport{
		ChangedFiles: changedFiles,
		Selected:     focusedArtifactSelections(selected),
		Commands:     cmds,
	}

	if len(cmds) == 0 {
		if err := e.recordFocusedChecksReport(taskID, step.ID, report); err != nil {
			e.logger.Warn("workflow.focused-checks.artifact", "task_id", taskID, "err", err)
		}
		return stepDone(step, "skipped: no safe focused mapping matched changed files")
	}

	timeout := resolveWorkflowCheckTimeout(e.verifyTimeout)
	if timeout != e.verifyTimeout && e.verifyTimeout > 0 {
		e.logger.Info("workflow.focused-checks.timeout-scaled",
			"task_id", taskID, "base", e.verifyTimeout.String(), "effective", timeout.String())
	}
	if e.verifyTimeout <= 0 && timeout != verifyChecksDefaultTimeout {
		e.logger.Info("workflow.focused-checks.timeout-scaled",
			"task_id", taskID, "base", verifyChecksDefaultTimeout.String(), "effective", timeout.String())
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	maybeMiseTrust(ctx, wtPath)
	failedCmd, output, runErr := e.runVerifyCommands(ctx, taskID, wtPath, cmds)

	report.FailedCmd = failedCmd
	report.OutputTail = output
	if err := e.recordFocusedChecksReport(taskID, step.ID, report); err != nil {
		e.logger.Warn("workflow.focused-checks.artifact", "task_id", taskID, "err", err)
	}

	if runErr != nil {
		if errors.Is(runErr, context.Canceled) && e.ctx.Err() != nil {
			e.logger.Warn("workflow.focused-checks.canceled", "task_id", taskID, "err", runErr)
			return stepDone(step, "skipped: context canceled")
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			reason := fmt.Sprintf("focused checks exceeded the time budget (%s) before the author loop stabilized", timeout)
			return e.flagFocusedChecks(taskID, step, reason, "timeout")
		}
		reason := "focused checks could not run cleanly: " + trimDiffLine(runErr.Error())
		return e.flagFocusedChecks(taskID, step, reason, "setup")
	}
	if failedCmd == "" {
		e.recordEvidence(taskID, step.ID, evidenceCriterionFocusedChecks, evidence.ProofDeterministicCheck,
			0, strings.Join(cmds, " && "), report.OutputTail)
		return stepDone(step, "clean")
	}
	return e.reaskFocusedChecks(taskID, step, wfExec, t, selected, changedFiles, failedCmd, output)
}

type worktreeBaseRefGetter interface {
	WorktreeBaseRef(ctx context.Context, taskID string) string
}

func (e *Engine) focusedChecksBaseRef(taskID string) string {
	if getter, ok := e.checks.(worktreeBaseRefGetter); ok {
		return getter.WorktreeBaseRef(e.ctx, taskID)
	}
	return project.WorktreeBaseRefFresh
}

func changedFilesSinceProjectBase(parentCtx context.Context, wtPath, worktreeBaseRef string) ([]string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()
	base := resolveProjectBase(ctx, wtPath, worktreeBaseRef)
	out, err := gitCombinedOutput(ctx, wtPath, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var changed []string
	seen := map[string]bool{}
	for raw := range strings.SplitSeq(string(out), "\n") {
		file := strings.TrimSpace(raw)
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		changed = append(changed, file)
	}
	return changed, nil
}

func resolveProjectBase(ctx context.Context, wtPath, worktreeBaseRef string) string {
	if worktreeBaseRef == project.WorktreeBaseRefHead {
		if base := resolveLocalDefaultBranchBase(ctx, wtPath); base != "" {
			return base
		}
	}
	return resolveOriginBase(ctx, wtPath)
}

func resolveLocalDefaultBranchBase(ctx context.Context, wtPath string) string {
	branches := []string{}
	if out, err := gitStdout(ctx, wtPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch := strings.TrimPrefix(out, "origin/")
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	branches = append(branches, "master", "main")
	for _, branch := range branches {
		candidate := "refs/heads/" + branch
		if gitOK(ctx, wtPath, "rev-parse", "--verify", candidate) {
			return candidate
		}
	}
	return ""
}

func selectFocusedChecks(focused []project.FocusedCheck, changedFiles []string) (selected []selectedFocusedCheck, commands []string) {
	seenCmd := map[string]bool{}
	for _, raw := range focused {
		fc, ok := normalizeFocusedCheck(raw)
		if !ok {
			continue
		}
		matched := focusedMatchedFiles(fc, changedFiles)
		if len(matched) == 0 {
			continue
		}
		selected = append(selected, selectedFocusedCheck{
			FocusedCheck: fc,
			ChangedFiles: matched,
		})
		for _, cmd := range fc.Commands {
			if seenCmd[cmd] {
				continue
			}
			seenCmd[cmd] = true
			commands = append(commands, cmd)
		}
	}
	return selected, commands
}

func normalizeFocusedCheck(raw project.FocusedCheck) (project.FocusedCheck, bool) {
	out := project.FocusedCheck{Name: strings.TrimSpace(raw.Name)}
	for _, p := range raw.Paths {
		p = strings.TrimSpace(p)
		if safeRepoPattern(p) {
			out.Paths = append(out.Paths, p)
		}
	}
	for _, pkg := range raw.Packages {
		pkg = strings.TrimSpace(pkg)
		if safePackagePattern(pkg) {
			out.Packages = append(out.Packages, pkg)
		}
	}
	for _, cmd := range raw.Commands {
		cmd = strings.TrimSpace(cmd)
		if cmd != "" {
			out.Commands = append(out.Commands, cmd)
		}
	}
	if len(out.Commands) == 0 {
		return project.FocusedCheck{}, false
	}
	if len(out.Paths) == 0 && len(out.Packages) == 0 {
		return project.FocusedCheck{}, false
	}
	return out, true
}

func safeRepoPattern(pattern string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return false
	}
	for part := range strings.SplitSeq(pattern, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func safePackagePattern(pattern string) bool {
	if pattern == "." {
		return true
	}
	if !strings.HasPrefix(pattern, "./") {
		return false
	}
	base := strings.TrimSuffix(pattern, "/...")
	if base == "." || base == "./" || base == "" {
		return false
	}
	for part := range strings.SplitSeq(strings.TrimPrefix(base, "./"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func focusedMatchedFiles(fc project.FocusedCheck, changedFiles []string) []string {
	var matched []string
	for _, file := range changedFiles {
		if focusedMatchesPath(fc.Paths, file) || focusedMatchesPackage(fc.Packages, file) {
			matched = append(matched, file)
		}
	}
	return matched
}

func focusedMatchesPath(patterns []string, file string) bool {
	for _, pattern := range patterns {
		if matchRepoPath(pattern, file) {
			return true
		}
	}
	return false
}

func focusedMatchesPackage(patterns []string, file string) bool {
	pkg := packageForFile(file)
	for _, pattern := range patterns {
		if matchPackagePattern(pattern, pkg) {
			return true
		}
	}
	return false
}

func packageForFile(file string) string {
	dir := path.Dir(file)
	if dir == "." {
		return "."
	}
	return "./" + dir
}

func matchPackagePattern(pattern, pkg string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/..."); ok {
		return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
	}
	return pkg == pattern
}

func matchRepoPath(pattern, name string) bool {
	patterns := strings.Split(pattern, "/")
	parts := strings.Split(name, "/")
	memo := make(map[repoPathMatchState]bool, len(patterns)*len(parts))
	return matchRepoPathSegments(patterns, parts, 0, 0, memo)
}

type repoPathMatchState struct {
	pattern int
	part    int
}

func matchRepoPathSegments(patterns, parts []string, patternIdx, partIdx int, memo map[repoPathMatchState]bool) bool {
	state := repoPathMatchState{pattern: patternIdx, part: partIdx}
	if matched, ok := memo[state]; ok {
		return matched
	}

	var matched bool
	switch {
	case patternIdx == len(patterns):
		matched = partIdx == len(parts)
	case patterns[patternIdx] == "**":
		if matchRepoPathSegments(patterns, parts, patternIdx+1, partIdx, memo) {
			matched = true
		} else {
			matched = partIdx < len(parts) && matchRepoPathSegments(patterns, parts, patternIdx, partIdx+1, memo)
		}
	case partIdx == len(parts):
		matched = false
	default:
		ok, err := path.Match(patterns[patternIdx], parts[partIdx])
		matched = err == nil && ok && matchRepoPathSegments(patterns, parts, patternIdx+1, partIdx+1, memo)
	}

	memo[state] = matched
	return matched
}

func focusedArtifactSelections(selected []selectedFocusedCheck) []focusedCheckArtifact {
	out := make([]focusedCheckArtifact, 0, len(selected))
	for _, s := range selected {
		out = append(out, focusedCheckArtifact{
			Name:         s.FocusedCheck.Name,
			Paths:        slices.Clone(s.FocusedCheck.Paths),
			Packages:     slices.Clone(s.FocusedCheck.Packages),
			ChangedFiles: slices.Clone(s.ChangedFiles),
		})
	}
	return out
}

func (e *Engine) recordFocusedChecksReport(taskID, stepID string, report focusedChecksReport) error {
	if e.recorder == nil {
		return nil
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return e.recorder.PutGeneric(taskID, "focused-checks.json", stepID, string(data))
}

func (e *Engine) flagFocusedChecks(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("focused-checks: set human-required: %w", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionFocusedChecks, evidence.ProofDeterministicCheck, 1, "", detail)
	e.logger.Warn("workflow.focused-checks.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}

func (e *Engine) reaskFocusedChecks(taskID string, step *Step, wfExec *Execution, t TaskInfo, selected []selectedFocusedCheck, changedFiles []string, failedCmd, output string) (StepOutput, error) {
	reason := "focused checks failed"
	if surfaces := focusedSurfaceSummary(selected); surfaces != "" {
		reason += " for " + surfaces
	}
	reason += ": " + trimDiffLine(failedCmd)
	if wfExec == nil || wfExec.CountStep(verifyChecksImplStepID) == 0 {
		return e.flagFocusedChecks(taskID, step, reason, failedCmd)
	}
	fingerprint := autoFixFailureFingerprint(failedCmd, output)
	armed, attempt, err := e.rewindRetry(taskID, wfExec, t, rewindRetryPolicy{
		counterKey:  "step." + step.ID + ".auto_fix",
		max:         verifyChecksAutoFixCeiling,
		rewindStep:  verifyChecksImplStepID,
		backoff:     autoFixBackoff,
		fingerprint: fingerprint,
		// One prior identical occurrence means this is repeat #2. Re-running
		// the same deterministic failure again only spends another author run.
		maxSameFingerprintRuns: 1,
		attemptProducedWork:    lastAuthorRunProducedWork,
		onArm: func(wfExec *Execution, attempt int) {
			wfExec.SetVar(focusedChecksReaskNoteVar, buildFocusedChecksReaskNote(selected, changedFiles, failedCmd, output))
			wfExec.SetVar(verifyRetryModelVar, "expensive")
		},
		reason: func(int) string { return reason },
	})
	if err != nil {
		return StepOutput{}, fmt.Errorf("focused-checks: rewind to implement: %w", err)
	}
	if !armed {
		exhausted := fmt.Sprintf("%s — escalating after repeated identical auto-fix failures or %d attempts without passing",
			reason, verifyChecksAutoFixCeiling)
		return e.flagFocusedChecks(taskID, step, exhausted, "auto-fix-exhausted: "+trimDiffLine(failedCmd))
	}
	e.logger.Info("workflow.focused-checks.reask",
		"task_id", taskID, "attempt", attempt, "cmd", trimDiffLine(failedCmd))
	return StepOutput{}, errStepParked
}

func buildFocusedChecksReaskNote(selected []selectedFocusedCheck, changedFiles []string, failedCmd, output string) string {
	var b strings.Builder
	b.WriteString("A prior implementation FAILED Sybra's focused checks. Fix the ROOT CAUSE so the failing command passes on a clean run")
	b.WriteString(" — do NOT weaken, skip, or edit the check to make it pass. Then COMMIT and push your fix: the check runs against your")
	b.WriteString(" branch HEAD in a freshly prepared worktree, so an uncommitted change is not picked up and the same failure recurs;")
	b.WriteString(" some projects also enforce a clean working tree (e.g. a `git diff --exit-code` / generated-file gate) that fails outright on uncommitted changes.\n\n")
	if surfaces := focusedSurfaceSummary(selected); surfaces != "" {
		b.WriteString("## Focused surface\n\n")
		b.WriteString(surfaces)
		b.WriteString("\n\n")
	}
	if len(changedFiles) > 0 {
		b.WriteString("## Changed files\n\n")
		for _, file := range changedFiles {
			b.WriteString("- `")
			b.WriteString(file)
			b.WriteString("`\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("## Failing command\n\n`")
	b.WriteString(failedCmd)
	b.WriteString("`\n\n## Output (tail)\n\n```\n")
	b.WriteString(tailString(strings.TrimSpace(output), 3000))
	b.WriteString("\n```")
	return b.String()
}

func focusedSurfaceSummary(selected []selectedFocusedCheck) string {
	var labels []string
	for _, s := range selected {
		label := strings.TrimSpace(s.FocusedCheck.Name)
		if label == "" {
			switch {
			case len(s.FocusedCheck.Packages) > 0:
				label = strings.Join(s.FocusedCheck.Packages, ", ")
			case len(s.FocusedCheck.Paths) > 0:
				label = strings.Join(s.FocusedCheck.Paths, ", ")
			}
		}
		if label != "" {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, ", ")
}
