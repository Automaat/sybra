package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// verifyChecksDefaultTimeout bounds the whole verify run (every command). A test
// suite is slower than the 30s shellTimeout used by lighter steps. Overridable
// per-engine via SetVerifyTimeout (tests use a short value).
const verifyChecksDefaultTimeout = 10 * time.Minute

// verifyChecksMaxOutput bounds how much command output is retained in memory.
// A noisy or malicious verify command must not be able to OOM the engine, so
// output streams into a fixed-size tail buffer rather than a growing slice.
const verifyChecksMaxOutput = 64 * 1024

// verifyChecksOutputTail caps how much of that buffer is stored in the artifact.
const verifyChecksOutputTail = 8000

// verifyBlessedTag lets a human accept a verify failure (e.g. a known-flaky
// suite) and let the task proceed instead of re-blocking on every re-dispatch.
const verifyBlessedTag = "verify-blessed"

// verifyChecksFlakeRetries is how many extra times a failed verify command is
// re-run before the gate blocks. A single retry absorbs a nondeterministic
// flake (e.g. a test-teardown TempDir race) that would otherwise escalate
// unrelated work to human-required. A genuine failure fails every attempt and
// still blocks; the cost is one extra suite run only on the failing command.
const verifyChecksFlakeRetries = 1

const verifyChecksTimeoutRetries = 1

// verifyChecksNodeModulesRepairTimeout bounds a single install repair run
// (see repairIncompleteNodeModules). Independent of verifyTimeout: repair is
// a one-off best-effort side step, not part of the suite's own budget.
const verifyChecksNodeModulesRepairTimeout = 5 * time.Minute

// nodeInstallManager describes how to detect and repair a half-installed
// node_modules for one package manager. installMarkers are the files a
// *successful* install writes as its final step, relative to node_modules;
// their absence (with the lockfile present) is the signature of an install
// interrupted mid-reify (issue b3ec32b3). Detection keys off the completion
// marker rather than `.bin` because a legitimate install whose dependency
// tree provides no CLI binaries never creates `.bin`, which would otherwise
// misclassify it as permanently incomplete.
type nodeInstallManager struct {
	lockfile       string
	installMarkers []string
	repairCmd      string
}

// nodeInstallManagers is ordered by precedence: the first lockfile found in a
// directory picks the manager. Each repairCmd is a deterministic
// install-from-lockfile invocation.
var nodeInstallManagers = []nodeInstallManager{
	{lockfile: "package-lock.json", installMarkers: []string{".package-lock.json"}, repairCmd: "npm ci"},
	{lockfile: "pnpm-lock.yaml", installMarkers: []string{".modules.yaml"}, repairCmd: "pnpm install --frozen-lockfile"},
	{lockfile: "yarn.lock", installMarkers: []string{".yarn-state.yml", ".yarn-integrity"}, repairCmd: "yarn install --frozen-lockfile"},
}

// nodeRepairTarget is a directory with an incomplete install and the command
// that reinstalls it from its lockfile.
type nodeRepairTarget struct {
	dir string
	cmd string
}

// nodeModulesRepairMaxDepth bounds how deep the corrupted-node_modules scan
// descends into the worktree, so a repo with a deep source layout doesn't
// pay for a full-tree walk on every verify run.
const nodeModulesRepairMaxDepth = 3

// nodeModulesRepairTimeoutCap bounds the best-effort npm repair step. The
// verify suite gets a larger budget because it is the project-declared gate;
// repair is only an infra recovery attempt before retrying the failed command.
const nodeModulesRepairTimeoutCap = 3 * time.Minute

var verifyMissingToolchainRe = regexp.MustCompile(
	`(?i)command not found|executable file not found|not found in \$?PATH|[\w./-]+: not found`)

// verifyChecksReport is the structured result, stored as a generic artifact.
type verifyChecksReport struct {
	Commands              []string `json:"commands"`
	FailedCmd             string   `json:"failedCmd,omitempty"`
	OutputTail            string   `json:"outputTail,omitempty"`
	NodeModulesRepairDirs []string `json:"nodeModulesRepairDirs,omitempty"`
	RepairSucceeded       bool     `json:"repairSucceeded,omitempty"`
}

// execVerifyChecks runs the project's deterministic verify suite
// (`checks.verify`, opt-in) in the agent's worktree before it hands off to
// review. A non-zero exit flips the task to human-required so an implementation
// that does not pass its own declared verification suite cannot reach a PR.
// Complements detect_tampering (phase 1): structural test tampering is caught
// there; this catches incomplete/broken work the agent committed without the
// suite passing.
//
// The commands run in the agent's already-set-up worktree with no git mutation,
// reusing the same `sh -c` + inherited-env mechanism as worktree setup so the
// toolchain (mise) resolves identically.
//
// Short-circuits (no block): the verify-blessed tag, no CheckConfigGetter, no
// verify commands configured, or no worktree. A genuine command failure — or
// the suite exceeding the time budget (an agent could hang a test to dodge) —
// blocks. Only engine-shutdown cancellation fails open, so a harness problem on
// teardown never strands work.
//
// A failure first gets one honest re-check: if the worktree has a
// structurally corrupted node_modules (see findCorruptedNodeModules — the
// signature of a killed npm install, not a code regression), the gate runs
// `npm ci` to repair it and retries the failing command before blocking.
// This distinguishes "toolchain broken by host load" from an actual
// implementation defect, so a Go-only diff doesn't get parked to
// human-required by an unrelated frontend infra hiccup.
func (e *Engine) execVerifyChecks(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if slices.Contains(t.Tags, verifyBlessedTag) {
		e.logger.Info("workflow.verify-checks.blessed", "task_id", taskID)
		return stepDone(step, "blessed")
	}
	if e.checks == nil {
		return stepDone(step, "skipped: no check config getter")
	}

	timeout := e.verifyTimeout
	if timeout <= 0 {
		timeout = verifyChecksDefaultTimeout
	}
	cmds := e.checks.VerifyCommands(e.ctx, taskID)
	if len(cmds) == 0 {
		return stepDone(step, "skipped: no verify commands configured")
	}
	if e.worktrees == nil {
		return stepDone(step, "skipped: no worktree getter configured")
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return stepDone(step, "skipped: no worktree for task")
	}

	failedCmd, failedIndex, output, runErr := e.runVerifySuiteWithRetry(taskID, wtPath, cmds, timeout, step.ID)

	var repairDirs []string
	repairSucceeded := false
	if failedCmd != "" && runErr == nil {
		repairDirs = findCorruptedNodeModules(verifyCommandScanRoot(wtPath, failedCmd))
		if len(repairDirs) > 0 {
			var repairErr error
			repairSucceeded, repairErr = e.repairCorruptedNodeModules(taskID, repairDirs, nodeModulesRepairTimeout(timeout))
			if repairErr != nil {
				runErr = repairErr
			} else if repairSucceeded {
				retryCmds := cmds
				if failedIndex >= 0 {
					retryCmds = cmds[failedIndex:]
				}
				retryFailed, _, retryOutput, retryErr :=
					e.runVerifySuiteWithRetry(taskID, wtPath, retryCmds, timeout, step.ID)
				output += retryOutput
				if retryErr == nil {
					failedCmd, runErr = retryFailed, nil
				} else {
					runErr = retryErr
				}
			}
		}
	}

	// A real command failure (not a timeout/cancel) can be an artifact of a
	// half-installed node_modules rather than the diff under test — e.g. a
	// backgrounded install reaped mid-run when its parent agent process was
	// torn down (issue b3ec32b3) leaves package dirs present but the install
	// unfinalized, so any devDependency binary the suite shells out to (vite,
	// tsc, eslint, …) reports "command not found" on an otherwise-correct diff.
	// Detection keys off the missing install-completion marker, so only a
	// genuinely interrupted install triggers a repair. Repair once and re-run
	// before flagging.
	if runErr == nil && failedCmd != "" && e.repairIncompleteNodeModules(taskID, wtPath) {
		failedCmd, _, output, runErr = e.runVerifySuiteWithRetry(taskID, wtPath, cmds, timeout, step.ID)
	}

	if failedCmd != "" && verifyMissingToolchainRe.MatchString(output) {
		if healed, fc, out, rErr := e.healToolchainAndRetry(taskID, wtPath, cmds, timeout, step.ID); healed {
			failedCmd, output, runErr = fc, out, rErr
		}
	}

	report := verifyChecksReport{
		Commands: cmds, FailedCmd: failedCmd, OutputTail: tailString(output, verifyChecksOutputTail),
		NodeModulesRepairDirs: repairDirs, RepairSucceeded: repairSucceeded,
	}
	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "verify-checks.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.verify-checks.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}

	// Engine-shutdown cancellation is the only fail-open: there is no point
	// blocking work the engine is tearing down. Our own deadline fails CLOSED —
	// otherwise an agent could hang a test past the budget to dodge the gate.
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			reason := fmt.Sprintf(
				"verify suite exceeded the time budget (%s) on all %d attempts"+
					" — fix slow or hanging tests, or add the `verify-blessed` tag to override",
				timeout, verifyChecksTimeoutRetries+1)
			return e.flagVerifyChecks(taskID, step, reason, "timeout")
		}
		e.logger.Warn("workflow.verify-checks.canceled", "task_id", taskID, "err", runErr)
		return stepDone(step, "skipped: context canceled")
	}

	if failedCmd != "" {
		reason := "implementation does not pass the project verify suite: " + trimDiffLine(failedCmd) +
			" — fix the code, or add the `verify-blessed` tag to override (e.g. a known-flaky suite)"
		return e.flagVerifyChecks(taskID, step, reason, failedCmd)
	}

	e.logger.Info("workflow.verify-checks.clean", "task_id", taskID, "commands", len(cmds))
	return stepDone(step, "clean")
}

// flagVerifyChecks flips the task to human-required. A failed status write
// returns an error so the workflow stalls instead of advancing past the gate —
// the YAML transition keys off task.status, so a silently-failed write would
// otherwise route a failing implementation straight to review.
func (e *Engine) runVerifySuiteWithRetry(
	taskID, wtPath string,
	cmds []string,
	timeout time.Duration,
	stepID string,
) (failedCmd string, failedIndex int, output string, runErr error) {
	failedIndex = -1
	for attempt := 0; attempt <= verifyChecksTimeoutRetries; attempt++ {
		ctx, cancel := context.WithTimeout(e.ctx, timeout)
		maybeMiseTrust(ctx, wtPath)
		failedCmd, failedIndex, output, runErr = e.runVerifyCommands(ctx, taskID, wtPath, cmds)
		cancel()
		if !errors.Is(runErr, context.DeadlineExceeded) || e.ctx.Err() != nil {
			return failedCmd, failedIndex, output, runErr
		}
		if attempt < verifyChecksTimeoutRetries {
			e.logger.Warn("workflow.verify-checks.timeout-retry",
				"task_id", taskID, "step", stepID, "attempt", attempt+1, "budget", timeout.String())
		}
	}
	return failedCmd, failedIndex, output, runErr
}

func (e *Engine) healToolchainAndRetry(taskID, wtPath string, cmds []string, timeout time.Duration, stepID string) (attempted bool, failedCmd, output string, runErr error) {
	setupCtx, cancel := context.WithTimeout(e.ctx, timeout)
	setup := e.checks.SetupCommands(setupCtx, taskID)
	if len(setup) == 0 {
		cancel()
		return false, "", "", nil
	}
	e.logger.Warn("workflow.verify-checks.toolchain-heal",
		"task_id", taskID, "step", stepID, "setup_commands", len(setup))
	maybeMiseTrust(setupCtx, wtPath)
	sFailed, _, sOut, sErr := e.runVerifyCommands(setupCtx, taskID, wtPath, setup)
	cancel()
	if sFailed != "" || sErr != nil {
		e.logger.Warn("workflow.verify-checks.toolchain-heal.setup-failed",
			"task_id", taskID, "cmd", trimDiffLine(sFailed), "err", sErr, "tail", tailString(sOut, 400))
		return false, "", "", nil
	}
	failedCmd, _, output, runErr = e.runVerifySuiteWithRetry(taskID, wtPath, cmds, timeout, stepID)
	e.logger.Info("workflow.verify-checks.toolchain-heal.reran",
		"task_id", taskID, "step", stepID, "still_failing", failedCmd != "" || runErr != nil)
	return true, failedCmd, output, runErr
}

func (e *Engine) flagVerifyChecks(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("verify-checks: set human-required: %w", statusErr)
	}
	e.logger.Warn("workflow.verify-checks.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}

// repairTornNodeModulesTimeout bounds the best-effort `npm ci` repair below,
// separate from the verify budget so a repair can never eat into the time
// the actual verify commands get.
const repairTornNodeModulesTimeout = 3 * time.Minute

// repairTornNodeModules scans the worktree root and immediate subdirectories
// for an npm project (a package.json) whose node_modules looks torn and repairs
// it with a single `npm ci --ignore-scripts` before verify commands trust the
// install. This is a defensive fix for a race between verify_checks and a
// still-running (or killed mid-flight) `npm ci` left behind by something else
// in the same worktree, such as an implementation agent's own backgrounded
// `mise run verify`/`npm ci` that its session never waited on: the setup-time
// install was healthy, but by the time verify_checks runs, node_modules can be
// left half torn-down, and every npm-run command in it then fails with
// "command not found" — a false implementation-defect signal. Best-effort: a
// repair failure surfaces via the verify command's own failure, same as before
// this existed.
//
// Lifecycle scripts are deliberately disabled because package.json is
// branch-controlled; verify command configuration comes from the trusted base
// branch, and this best-effort repair must not introduce a new unsandboxed
// execution path controlled by the implementation branch.
//
// "Torn" mirrors the staleness check scripts/sybra-dev-supervisor.sh already
// uses for the desktop dev loop: node_modules/.bin missing (an install that
// never completed or was gutted mid-run), or npm's own
// node_modules/.package-lock.json stamp missing/older than package-lock.json
// (an install that was interrupted before npm finished writing its stamp).
func (e *Engine) repairTornNodeModules(taskID, wtPath string) {
	e.repairTornNodeModulesInDir(taskID, wtPath, ".")
	entries, err := os.ReadDir(wtPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(wtPath, entry.Name())
		e.repairTornNodeModulesInDir(taskID, dir, entry.Name())
	}
}

func (e *Engine) repairTornNodeModulesInDir(taskID, dir, label string) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err != nil {
		return // npm ci requires a lockfile; skip non-npm or lockfile-less projects
	}
	nodeModules := filepath.Join(dir, "node_modules")
	info, err := os.Stat(nodeModules)
	if err != nil || !info.IsDir() {
		return // no install yet — not this gate's problem
	}
	if !nodeModulesTorn(dir, nodeModules) {
		return
	}

	e.logger.Warn("workflow.verify-checks.npm-repair", "task_id", taskID, "dir", label)
	ctx, cancel := context.WithTimeout(e.ctx, repairTornNodeModulesTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		e.logger.Warn("workflow.verify-checks.npm-repair-failed", "task_id", taskID, "dir", label, "err", err)
	}
}

// nodeModulesTorn reports whether nodeModules (an existing directory inside
// dir) looks like an install that never finished cleanly.
func nodeModulesTorn(dir, nodeModules string) bool {
	binInfo, err := os.Stat(filepath.Join(nodeModules, ".bin"))
	if err != nil || !binInfo.IsDir() {
		return true
	}
	lockPath := filepath.Join(dir, "package-lock.json")
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		return false // no lockfile to compare against — .bin presence is all we can check
	}
	stampInfo, err := os.Stat(filepath.Join(nodeModules, ".package-lock.json"))
	if err != nil {
		return true // npm never finished writing its own stamp
	}
	return stampInfo.ModTime().Before(lockInfo.ModTime())
}

// maybeMiseTrust trusts a mise config in the worktree before running verify
// commands, mirroring worktree setup. A task that adds or edits mise config
// would otherwise hit "config not trusted" and fail verify on honest work.
// Best-effort: errors are ignored (the verify command surfaces any real issue).
func maybeMiseTrust(ctx context.Context, wtPath string) {
	for _, name := range []string{"mise.toml", ".mise.toml", "mise.local.toml"} {
		if _, err := os.Stat(filepath.Join(wtPath, name)); err == nil {
			cmd := exec.CommandContext(ctx, "sh", "-c", "mise trust --yes")
			cmd.Dir = wtPath
			_ = cmd.Run()
			return
		}
	}
}

// repairIncompleteNodeModules re-runs the lockfile install for every node
// project directory under wtPath whose node_modules looks half-installed (see
// findIncompleteNodeModulesDirs). Returns true only when at least one repair
// actually succeeded — a caller retries the verify suite only if the toolchain
// might now be fixed; retrying after every repair failed just burns the suite
// budget on a state known not to have changed. Best-effort: a failed install
// here is not itself a verify failure — the retried verify run surfaces the
// real error if repair did not fix it.
func (e *Engine) repairIncompleteNodeModules(taskID, wtPath string) bool {
	targets := findIncompleteNodeModulesDirs(wtPath)
	if len(targets) == 0 {
		return false
	}
	repaired := false
	for _, target := range targets {
		e.logger.Warn("workflow.verify-checks.node-modules-incomplete",
			"task_id", taskID, "dir", target.dir, "cmd", target.cmd)
		ctx, cancel := context.WithTimeout(e.ctx, verifyChecksNodeModulesRepairTimeout)
		cmd := exec.CommandContext(ctx, "sh", "-c", target.cmd)
		cmd.Dir = target.dir
		out, repairErr := cmd.CombinedOutput()
		cancel()
		if repairErr != nil {
			e.logger.Warn("workflow.verify-checks.node-modules-repair-failed",
				"task_id", taskID, "dir", target.dir, "err", repairErr, "tail", tailString(string(out), 400))
			continue
		}
		repaired = true
		e.logger.Info("workflow.verify-checks.node-modules-repaired", "task_id", taskID, "dir", target.dir)
	}
	return repaired
}

// findIncompleteNodeModulesDirs walks wtPath for node project directories
// (a supported lockfile present) whose node_modules exists but is missing its
// install-completion marker — the signature of an install interrupted after
// packages were unpacked but before the package manager finalized the tree.
// Never descends into node_modules itself (nested lockfiles inside
// dependencies are not our project's install).
func findIncompleteNodeModulesDirs(wtPath string) []nodeRepairTarget {
	var targets []nodeRepairTarget
	walkErr := filepath.WalkDir(wtPath, func(path string, d fs.DirEntry, statErr error) error {
		if statErr != nil {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" || d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if mgr, ok := incompleteNodeModulesManager(path); ok {
			targets = append(targets, nodeRepairTarget{dir: path, cmd: mgr.repairCmd})
		}
		return nil
	})
	_ = walkErr // best-effort scan: a walk error just means less coverage, not a failure
	return targets
}

// incompleteNodeModulesManager reports the package manager for dir when dir is
// a node project (a supported lockfile present) whose node_modules exists but
// is missing every install-completion marker for that manager — the signature
// of an install interrupted before it finalized the dependency tree. The
// marker check (not `.bin` presence) avoids misclassifying a legitimately
// complete install whose dependencies provide no CLI binaries.
func incompleteNodeModulesManager(dir string) (nodeInstallManager, bool) {
	for _, mgr := range nodeInstallManagers {
		if _, err := os.Stat(filepath.Join(dir, mgr.lockfile)); err != nil {
			continue
		}
		nmPath := filepath.Join(dir, "node_modules")
		nmInfo, err := os.Stat(nmPath)
		if err != nil || !nmInfo.IsDir() {
			return nodeInstallManager{}, false // no install yet — not our concern, not a repair target
		}
		for _, marker := range mgr.installMarkers {
			if _, err := os.Stat(filepath.Join(nmPath, marker)); err == nil {
				return nodeInstallManager{}, false // install completed — nothing to repair
			}
		}
		return mgr, true
	}
	return nodeInstallManager{}, false
}

// findCorruptedNodeModules scans rootPath (bounded depth) for npm-project
// directories whose node_modules was left in a structurally broken state —
// the signature of an `npm ci`/install killed mid-write (e.g. by host memory
// pressure reaping the process): node_modules exists but is missing .bin/ or
// .package-lock.json, so any bin shim (vite, tsc, ...) resolves to "command
// not found" even though the code under test never touched the frontend. A
// wholesale-missing node_modules (never installed) is left alone — that is a
// setup-command problem, not something this gate should paper over.
func findCorruptedNodeModules(rootPath string) []string {
	var corrupted []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > nodeModulesRepairMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		hasPackageJSON := false
		hasPackageLock := false
		for _, entry := range entries {
			if entry.IsDir() || isSymlinkDirEntry(entry) {
				continue
			}
			switch entry.Name() {
			case "package.json":
				hasPackageJSON = true
			case "package-lock.json":
				hasPackageLock = true
			}
		}
		for _, entry := range entries {
			if isSymlinkDirEntry(entry) || !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if name == ".git" {
				continue
			}
			if name == "node_modules" {
				if hasPackageJSON && hasPackageLock && isCorruptedNodeModules(filepath.Join(dir, name)) {
					corrupted = append(corrupted, dir)
				}
				continue // never descend into node_modules itself
			}
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(rootPath, 0)
	return corrupted
}

func isSymlinkDirEntry(entry os.DirEntry) bool {
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

func verifyCommandScanRoot(wtPath, rawCmd string) string {
	dir, ok := leadingShellCD(rawCmd)
	if !ok {
		return wtPath
	}
	clean := filepath.Clean(strings.Trim(dir, `"'`))
	if clean == "." || filepath.IsAbs(clean) {
		return wtPath
	}
	root := filepath.Join(wtPath, clean)
	rel, err := filepath.Rel(wtPath, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return wtPath
	}
	return root
}

func leadingShellCD(rawCmd string) (string, bool) {
	cmd := strings.TrimSpace(rawCmd)
	if !strings.HasPrefix(cmd, "cd ") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "cd "))
	for _, sep := range []string{"&&", ";"} {
		if before, _, ok := strings.Cut(rest, sep); ok {
			dir := strings.TrimSpace(before)
			return dir, dir != ""
		}
	}
	return "", false
}

// isCorruptedNodeModules reports whether nodeModulesPath looks like a
// partial install rather than a complete one.
func isCorruptedNodeModules(nodeModulesPath string) bool {
	info, err := os.Lstat(nodeModulesPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(nodeModulesPath, ".bin")); err != nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(nodeModulesPath, ".package-lock.json")); err != nil {
		return true
	}
	return false
}

// repairCorruptedNodeModules runs a clean `npm ci` in each directory whose
// node_modules looked structurally broken, giving the failed verify command
// one honest shot against a complete toolchain before the gate blocks the
// task. Returns false — no repair credited — if any directory's `npm ci`
// itself fails, since that signals a real problem (missing lockfile, no
// network) rather than a recoverable partial install; the caller then falls
// through to its normal fail-and-block path instead of masking it.
func (e *Engine) repairCorruptedNodeModules(taskID string, dirs []string, timeout time.Duration) (bool, error) {
	repaired := true
	for _, dir := range dirs {
		if ctxErr := e.ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		e.logger.Warn("workflow.verify-checks.node-modules-corrupted", "task_id", taskID, "dir", dir)
		ctx, cancel := context.WithTimeout(e.ctx, timeout)
		maybeMiseTrust(ctx, dir)
		cmd := exec.CommandContext(ctx, "npm", "ci")
		cmd.Dir = dir
		tail := &boundedTail{max: verifyChecksMaxOutput}
		cmd.Stdout = tail
		cmd.Stderr = tail
		runErr := cmd.Run()
		out := tail.String()
		ctxErr := e.ctx.Err()
		cancel()
		if ctxErr != nil {
			return false, ctxErr
		}
		if runErr != nil {
			e.logger.Warn("workflow.verify-checks.node-modules-repair-failed",
				"task_id", taskID, "dir", dir, "err", runErr, "output", tailString(out, 2000))
			repaired = false
			continue
		}
		e.logger.Info("workflow.verify-checks.node-modules-repaired", "task_id", taskID, "dir", dir)
	}
	return repaired, nil
}

func nodeModulesRepairTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > nodeModulesRepairTimeoutCap {
		return nodeModulesRepairTimeoutCap
	}
	return timeout
}

func stepDone(step *Step, output string) (StepOutput, error) {
	return StepOutput{StepID: step.ID, Status: "completed", Output: output}, nil
}

// runVerifyCommands runs each command in order in the worktree via `sh -c`.
// Returns the first command that exited non-zero on every attempt (a real
// failure → caller blocks). A failing command is retried up to
// verifyChecksFlakeRetries times to absorb nondeterministic flakes; a command
// that passes on any attempt moves on. A non-nil err means the run could not
// complete (ctx timeout/cancel) and is never retried — the budget is already
// spent; the caller decides the policy (fail closed on our deadline, open on
// shutdown). Output streams into a fixed-size tail buffer so a flood of
// stdout/stderr cannot exhaust memory.
func (e *Engine) runVerifyCommands(
	ctx context.Context,
	taskID, wtPath string,
	cmds []string,
) (failedCmd string, failedIndex int, output string, err error) {
	tail := &boundedTail{max: verifyChecksMaxOutput}
	for idx, raw := range cmds {
		passed := false
		for attempt := 0; attempt <= verifyChecksFlakeRetries; attempt++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", -1, tail.String(), ctxErr
			}
			if attempt > 0 {
				_, _ = fmt.Fprintf(tail,
					"[verify] command failed; retry %d/%d to rule out a flake\n", attempt, verifyChecksFlakeRetries)
				e.logger.Info("workflow.verify-checks.retry",
					"task_id", taskID, "attempt", attempt, "cmd", trimDiffLine(raw))
			}
			_, _ = io.WriteString(tail, "$ "+raw+"\n")
			cmd := exec.CommandContext(ctx, "sh", "-c", raw)
			cmd.Dir = wtPath
			cmd.Stdout = tail
			cmd.Stderr = tail
			runErr := cmd.Run()
			_, _ = io.WriteString(tail, "\n")
			if runErr == nil {
				passed = true
				break // passed (possibly on retry) — go to the next command
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", -1, tail.String(), ctxErr // deadline/cancel: do not retry
			}
		}
		if !passed {
			return raw, idx, tail.String(), nil // failed every attempt → block
		}
	}
	return "", -1, tail.String(), nil
}

// boundedTail is a concurrency-safe io.Writer that retains only the last `max`
// bytes written. os/exec writes stdout and stderr from separate goroutines when
// they share a non-*os.File writer, so Write must be guarded.
type boundedTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *boundedTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *boundedTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// tailString returns the last n bytes of s, prefixed with an elision marker
// when truncated. Debug output — not rune-aligned at the cut.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}
