package agent

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

// newProviderCmd is the sole constructor for a provider CLI subprocess. Every
// exec.CommandContext spawn of a provider binary (claude/codex/copilot/opencode, in
// any of headless-pipe, headless-survive, persistent-convo, convo-survive,
// or per-turn-convo mode) must go through here so a single seam can wrap the
// invocation for OS-level sandboxing (see wrapInvocation in
// procsandbox_darwin.go/procsandbox_other.go) — a new spawn site cannot
// obtain an unsandboxed provider process by construction.
//
// detached selects the shutdown-behavior seam: false configures graceful
// SIGTERM-then-SIGKILL shutdown on ctx cancellation (configureGracefulShutdown)
// for a live, pipe-backed spawn; true configures a detached process group
// (configureDetached) for a spawn that must survive a cancelled parent ctx —
// in that case the child is run under context.Background() instead of ctx, so
// a shutdown cancel never reaches it. cfg carries the sandbox spec computed by
// prepareRunConfig; a nil cfg spawns unwrapped (e.g. in tests that construct a
// command directly).
func newProviderCmd(ctx context.Context, cfg *RunConfig, detached bool, name string, args ...string) *exec.Cmd {
	name = canonicalProviderCommand(name, cfg)
	wrappedName, wrappedArgs := wrapInvocation(name, args, cfg)
	var cmd *exec.Cmd
	if detached {
		// no Context: a cancelled ctx must not kill a detached child
		// #nosec G702 -- wrappedName is the trusted sandbox/provider boundary.
		cmd = exec.CommandContext(context.Background(), wrappedName, wrappedArgs...) //nolint:contextcheck // detached child must survive a cancelled parent ctx
		configureDetached(cmd)
	} else {
		// #nosec G702 -- wrappedName is the trusted sandbox/provider boundary.
		cmd = exec.CommandContext(ctx, wrappedName, wrappedArgs...)
		configureGracefulShutdown(cmd)
	}
	return cmd
}

// canonicalProviderCommand launches an enforce-mode provider through its real
// executable rather than a symlink spelling. Some providers locate mandatory
// sibling helpers relative to argv[0]; Homebrew's Codex launcher is a symlink
// while codex-code-mode-host exists only beside its resolved target. Keep
// deterministic verifier commands untouched: cfg.provider identifies the
// provider selected for this run, while DisableVerifierControl marks commands
// such as git/tests that happen to share the process constructor.
func canonicalProviderCommand(name string, cfg *RunConfig) string {
	if cfg == nil || cfg.sandbox.mode != "enforce" || cfg.DisableVerifierControl || cfg.provider == nil || name != cfg.provider.Name() {
		return name
	}
	executable, err := exec.LookPath(name)
	if err != nil {
		return name
	}
	abs, err := filepath.Abs(executable)
	if err != nil {
		return name
	}
	resolved, err := canonicalizeRoot(abs)
	if err != nil {
		return name
	}
	return resolved
}

// sandboxSpec carries the resolved OS-level process-sandbox posture and
// canonicalized allowed write roots for one run, computed once by
// Manager.injectProcessSandbox (manager_run.go) and consumed by
// wrapInvocation (procsandbox_darwin.go / procsandbox_other.go) at each
// provider spawn site. The zero value ("") is equivalent to "off": unwrapped.
type sandboxSpec struct {
	// mode is "off" or "enforce" here — never "report": report-mode specs are
	// validated and logged by injectProcessSandbox but always stored as "off"
	// so a profile/SBPL defect can only affect an explicit enforce posture,
	// never the default rollout posture.
	mode        string
	worktree    string
	gitMetadata []string
	gitShared   []string
	gitReadonly []string
	sandboxHome string
	sidecarDir  string
	// stateDenied are paths re-locked read-only *after* the writable roots,
	// so one run cannot change how later runs behave. claude's state dir has
	// to stay writable — a real run writes plugins/, sessions/, session-env/,
	// shell-snapshots/ and projects/ — so the durable-config files are
	// carved back out rather than the directory being narrowed (#2779).
	stateDenied []string
	tmp         string
	// tmpAlias re-opens darwin's stable /tmp entrypoint for helpers that
	// bypass $TMPDIR and write there directly. Empty on platforms or runs
	// with no safe alias to add.
	tmpAlias    string
	sharedCache string
	// readOnlyDir, when non-empty, is re-locked read-only after every writable
	// root above is bound — see wrapInvocation. It must never be granted
	// through worktree/sandboxHome/tmp/sharedCache: those are broad roots (tmp
	// in particular is the whole system temp dir) that can legitimately
	// contain readOnlyDir as a subdirectory, and a bind mount only shadows an
	// ancestor's mount for paths bound *after* it.
	readOnlyDir  string
	profilePath  string
	gitAdminDir  string
	gitCommonDir string
	gitWorktrees string
	gitObjectDir string
	// gitLooseObjectPattern is Darwin's escaped Seatbelt regex granting only
	// canonical objects/<hex>/<hash> files. A subpath grant on gitObjectDir
	// would let a command redirect GIT_OBJECT_DIRECTORY to a nested store and
	// create maintenance packs there.
	gitLooseObjectPattern       string
	gitLooseObjectFanoutPattern string

	gitBranchRef    string
	gitBranchRefDir string
	gitBranchLogDir string
	// gitBranchRefFile/gitBranchRefLockFile/gitBranchLogFile are the exact,
	// single-file absolute paths for this task's own branch — darwin grants
	// these via literal (not subpath) SBPL rules, since gitBranchRefDir can be
	// shared with sibling tasks whose branch names nest under the same
	// directory (e.g. two "fix/..." branches both live under refs/heads/fix/).
	// Linux avoids that exposure with a bind-mounted overlay instead; Seatbelt
	// has no filesystem-view mechanism to do the same, so darwin narrows the
	// grant to the one file per root instead.
	gitBranchRefFile     string
	gitBranchRefLockFile string
	gitBranchLogFile     string
	// gitPackedRefsLockFile is needed by ordinary ref transactions such as
	// stash. The packed-refs data/new files remain ungranted, so maintenance
	// may acquire the lock but cannot publish a packed-ref rewrite.
	gitPackedRefsLockFile string
	// gitShallowFile/gitShallowLockFile: touched by a shallow fetch/clone
	// (`--depth`/`--shallow-since`) — not issued by Sybra's own git calls
	// today, but reachable if an agent runs one directly.
	gitShallowFile     string
	gitShallowLockFile string
	// gitStashRefFile/_LockFile/gitStashLogFile/_LockFile: refs/stash is a
	// single fixed-name ref directly under refs/, not per-branch — literal,
	// like the branch ref grants, since it sits at a known, exact path.
	gitStashRefFile         string
	gitStashRefLockFile     string
	gitStashLogFile         string
	gitStashLogLockFile     string
	gitRemoteRefDir         string
	gitRemoteLogDir         string
	gitRemoteLogLockPattern string
	gitTagRefDir            string
	gitTagLogDir            string
	gitTagLogLockPattern    string
	// gitNotesRefDir/gitNotesLogDir (refs/notes, logs/refs/notes): granted
	// as whole subpaths like remote/tag refs — repo-wide annotation truth,
	// not a task's own exclusive work.
	gitNotesRefDir         string
	gitNotesLogDir         string
	gitNotesLogLockPattern string
	gitOverlayObjectDir    string
	gitOverlayRefDir       string
	gitOverlayLogDir       string
	gitOverlayRefFile      string
	gitOverlayRemoteRefDir string
	gitOverlayRemoteLogDir string
	gitOverlayTagRefDir    string
	gitOverlayTagLogDir    string

	claudeState   string
	codexState    string
	copilotState  string
	opencodeState string
	toolCache     string
	// appSupport is the macOS per-user application-data root
	// (~/Library/Application Support). The codex CLI's in-process app-server
	// creates its own directory there at startup, which needs write on the
	// parent — granting only a codex-named subpath fails, measured. Without
	// it codex dies immediately under enforce with "failed to initialize
	// in-process app-server client: Operation not permitted". Empty off
	// darwin, where no such path exists.
	appSupport string
	// claudeScratch is the Claude Code per-user scratchpad root
	// (/tmp/claude-<uid>). Claude Code creates a per-session directory under
	// it and writes working files there, so without the grant every such
	// write fails EPERM and the agent retries an impossible operation
	// instead of progressing. On Linux this sits inside os.TempDir() and is
	// already covered; on darwin $TMPDIR is /var/folders/... while /tmp
	// resolves to /private/tmp, so it needs its own root.
	claudeScratch string

	// readRoots, when non-empty, switches the wrapper from "everything is
	// readable" to deny-by-default reads over exactly these roots (#2781).
	// Empty means reads stay unrestricted, which is the posture every
	// deployment has today. Every write root is also a read root; these are
	// the additional read-only ones (system, toolchain, project clone).
	//
	// The measured trap is ~/.local/share/mise/installs: 8403 unique reads in
	// the #2780 tracing pass, the largest non-worktree read root, because it
	// holds the Go stdlib source every build compiles against. It appears on
	// no command line, so it is invisible to log-derived allowlists.
	readRoots []string
}

// writeRoots returns every path this spec grants write access to, so the read
// allowlist can guarantee a writable root is never invisible. Empty entries
// are dropped by the caller's dedupeRoots.
func (s sandboxSpec) writeRoots() []string {
	roots := []string{
		s.worktree, s.sandboxHome, s.sidecarDir, s.tmp, s.tmpAlias, s.sharedCache,
		s.claudeState, s.codexState, s.copilotState, s.opencodeState, s.toolCache,
		s.appSupport, s.claudeScratch,
		s.gitAdminDir, s.gitCommonDir, s.gitWorktrees, s.gitObjectDir,
		s.gitOverlayObjectDir, s.gitOverlayRefDir, s.gitOverlayLogDir,
		s.gitOverlayRemoteRefDir, s.gitOverlayRemoteLogDir,
		s.gitOverlayTagRefDir, s.gitOverlayTagLogDir,
		s.gitBranchRefDir, s.gitBranchLogDir, s.gitRemoteRefDir,
		s.gitRemoteLogDir, s.gitTagRefDir, s.gitTagLogDir,
		s.gitNotesRefDir, s.gitNotesLogDir,
		s.gitBranchRefFile, s.gitBranchRefLockFile, s.gitBranchLogFile,
		s.gitStashRefFile, s.gitStashRefLockFile, s.gitStashLogFile, s.gitStashLogLockFile,
		s.gitShallowFile, s.gitShallowLockFile,
	}
	roots = append(roots, s.gitMetadata...)
	roots = append(roots, s.gitShared...)
	roots = append(roots, s.gitReadonly...)
	return roots
}

// attemptEventsFrom slices the events produced since prevLen out of all,
// clamping prevLen so a buffer that shrank between reads (should not happen,
// but is cheap to guard) never panics. Shared by the StreamEvent (headless)
// and ConvoEvent (conversational) attempt-scoping call sites.
func attemptEventsFrom[T any](all []T, prevLen int) []T {
	if prevLen > len(all) {
		prevLen = len(all)
	}
	return all[prevLen:]
}

// runRetryLoop drives the attempt/backoff/retry state machine shared by
// runHeadless and runConversational: it waits out headlessRetryBackoffs
// between attempts, invokes attempt once per iteration, and reacts to its
// (retry, err) result the same way regardless of provider mode.
//
// Returns true when the caller must return immediately without finalizing —
// either the app is shutting down on a detached, not-intentionally-stopped
// agent (leave it for the next reattach), the attempt left a detached
// subprocess running across shutdown (errSurviveShutdown), or the attempt
// returned a fatal error (already handled via m.handleError). Returns false
// when the loop broke out normally (or exhausted retries) and the caller
// should proceed to its own finalize step.
func (m *Manager) runRetryLoop(ctx context.Context, a *Agent, logTag string, attempt func(n int) (retry bool, err error)) (earlyReturn bool) {
	for n := range len(headlessRetryBackoffs) + 1 {
		if n > 0 {
			wait := headlessRetryBackoffs[n-1]
			m.logger.Info("agent."+logTag+".retry", "id", a.ID, "attempt", n, "backoff", wait)
			select {
			case <-ctx.Done():
				if a.isDetached() && !a.WasStopped() {
					return true
				}
				return false
			case <-time.After(wait):
			}
		}

		retry, fatalErr := attempt(n)
		if errors.Is(fatalErr, errSurviveShutdown) {
			return true
		}
		if fatalErr != nil {
			m.handleError(ctx, a, fatalErr)
			return true
		}
		if !retry {
			break
		}
		if n == len(headlessRetryBackoffs) {
			m.logger.Error("agent."+logTag+".retry.exhausted", "id", a.ID, "attempts", len(headlessRetryBackoffs))
		}
	}
	return false
}

// finalizeRun marks a completed run stopped, emits the terminal state/events,
// and releases the agent. Shared tail of runHeadless and runConversational
// once runRetryLoop has broken out normally.
func (m *Manager) finalizeRun(ctx context.Context, a *Agent, doneLogEvent string) {
	a.SetState(StateStopped)
	m.unregisterExecution(a.ID)
	m.logger.Info(doneLogEvent, "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(ctx, a, a.GetExitErr() == nil)
	m.markAgentDone(ctx, a)
}
