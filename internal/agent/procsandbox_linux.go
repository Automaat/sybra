//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// apparmorUserNamespaceSysctl is the Ubuntu 24.04 knob that denies
// unprivileged user namespaces to any binary without a matching AppArmor
// profile. It is named in the refusal because it is the single thing an
// operator has to act on, and bwrap's own error ("setting up uid map") does
// not point at it.
const apparmorUserNamespaceSysctl = "kernel.apparmor_restrict_unprivileged_userns"

// sandboxProbeTimeout bounds the probe. It returns in milliseconds on a
// healthy host; the bound only stops a wedged host from stalling dispatch. It
// is a variable so a test can shorten it.
var sandboxProbeTimeout = 15 * time.Second

// sandboxProbeWaitDelay bounds how long the probe waits for output after the
// deadline killed the wrapper. bwrap's own children inherit the pipe, so
// waiting on it alone never returns while a grandchild holds it open — the
// timeout would bound nothing.
const sandboxProbeWaitDelay = 2 * time.Second

// sandboxProbeRetryAfter is how long a probe failure stands before the host is
// probed again. Namespace denial is a static host property, but the same exec
// also fails transiently (namespace quota exhausted by concurrent runs, fork
// pressure, a probe that timed out under load). Caching a transient failure
// for the life of the process would refuse every later run on a host that
// recovered, so only success and a missing binary are cached outright.
const sandboxProbeRetryAfter = time.Minute

var (
	sandboxProbeMu sync.Mutex
	bwrapPath      string
	errBwrapProbe  error
	bwrapProbed    bool
	bwrapMissing   bool
	bwrapProbedAt  time.Time

	// sandboxProbeNow is a variable so a test can age a cached failure without
	// sleeping.
	sandboxProbeNow = time.Now

	// apparmorSysctlPath is a variable so a test can present a host that does
	// or does not restrict user namespaces without owning /proc.
	apparmorSysctlPath = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
)

// sandboxMechanismErr reports why this host cannot build a bwrap sandbox, or
// nil when it can. Presence on PATH is not sufficient evidence: bwrap only
// fails when it maps uids, so a host that denies unprivileged user namespaces
// carries a working binary that cannot produce a single sandbox. Probing that
// before dispatch turns one host problem into one refusal instead of a failed
// step per run.
//
// The lock is held across the probe so a burst of concurrent dispatches costs
// one spawn, not one each.
func sandboxMechanismErr() error {
	sandboxProbeMu.Lock()
	defer sandboxProbeMu.Unlock()
	if bwrapProbed && (errBwrapProbe == nil || bwrapMissing || sandboxProbeNow().Sub(bwrapProbedAt) < sandboxProbeRetryAfter) {
		return errBwrapProbe
	}
	bwrapProbed, bwrapProbedAt = true, sandboxProbeNow()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		bwrapPath, bwrapMissing = "", true
		errBwrapProbe = fmt.Errorf("bwrap is not on PATH: %w", err)
		return errBwrapProbe
	}
	bwrapPath, bwrapMissing = path, false
	errBwrapProbe = probeUserNamespace(path)
	return errBwrapProbe
}

// probeUserNamespace runs the cheapest sandbox that still exercises every
// namespace the wrapper asks for: the same --unshare-pid, --dev and --proc a
// real spawn carries, over a read-only bind of the host root. Probing a
// narrower sandbox than the one that will be built would certify a host that
// grants the user namespace and denies the rest.
//
// This is a host probe, not a provider spawn: it never carries a run's prompt,
// environment, or roots, and so is one of the documented non-provider
// exec.CommandContext sites that do not route through newProviderCmd.
func probeUserNamespace(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "--unshare-pid", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", probeTargetBinary())
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = sandboxProbeWaitDelay
	err := cmd.Run()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(out.String())
	if detail == "" {
		detail = err.Error()
	}
	if namespaceDenied(detail) && userNamespacesRestricted() {
		return fmt.Errorf("bwrap cannot create a namespace on this host (%s): %s=1 denies unprivileged user namespaces to binaries without an AppArmor profile; install a profile for bwrap or set the sysctl to 0", detail, apparmorUserNamespaceSysctl)
	}
	return fmt.Errorf("bwrap cannot build a sandbox on this host: %s", detail)
}

// probeTargetBinary resolves the trivial command the probe execs inside the
// sandbox to an absolute path. Passing a bare name would leave the probe at
// the mercy of the PATH bwrap resolves against, and an `execvp` failure there
// would be read as a host that cannot build a sandbox. The fallback is the
// path coreutils ships on every distribution Sybra runs on.
func probeTargetBinary() string {
	if path, err := exec.LookPath("true"); err == nil {
		return path
	}
	return "/bin/true"
}

// namespaceDenied reports whether bwrap failed at the namespace itself. Its
// exit status is 1 for every failure, including an unrelated one such as a
// missing target binary, so the sysctl is only named when the output shows the
// failure the sysctl actually causes — telling an operator to weaken a
// host-wide restriction to fix a PATH problem is worse than saying nothing.
func namespaceDenied(detail string) bool {
	for _, marker := range []string{"setting up uid map", "setting up gid map", "creating new namespace", "no permissions to creating new namespace"} {
		if strings.Contains(strings.ToLower(detail), marker) {
			return true
		}
	}
	return false
}

// userNamespacesRestricted reports whether the AppArmor sysctl that Ubuntu
// 24.04 enables by default is on. A host without the knob reads as false, so
// the refusal only names the sysctl where it is the plausible cause.
func userNamespacesRestricted() bool {
	raw, err := os.ReadFile(apparmorSysctlPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) != "0"
}

func sandboxWrapperName() string { return "bwrap" }

// Linux bind-mounts branch refs into an overlay and publishes them only after
// sandboxSyncShell has copied the corresponding objects into the shared
// store, so object staging is safe and required on this platform.
func sandboxUsesGitObjectOverlay() bool { return true }

func canonicalizeRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("sandbox: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	return resolved, nil
}

func sandboxTmpAlias(string) string { return "" }

func wrapInvocation(name string, args []string, cfg *RunConfig) (wrappedName string, wrappedArgs []string) {
	if cfg == nil || cfg.sandbox.mode != "enforce" {
		return name, args
	}
	wrapped := []string{
		// Without this, --proc /proc still reflects the host's real process
		// table: a sandboxed agent's own `pkill -f <pattern>` can see and
		// signal arbitrary host processes it doesn't own the tree of — e.g. a
		// test-runner's dev-server teardown reaping an unrelated sibling
		// sybra-server on the shared host, self-inflicting the very
		// completion-stall it was supposed to avoid. bwrap reaps zombies for
		// the new namespace's pid 1 automatically (see bwrap(1)), so no
		// additional init flag is needed.
		"--unshare-pid",
	}
	if len(cfg.sandbox.readRoots) == 0 {
		wrapped = append(wrapped, "--ro-bind", "/", "/")
	} else {
		// Deny-by-default reads (#2781): bind only the allowlisted roots
		// instead of the whole filesystem, so nothing else is even visible.
		// --ro-bind-try, not --ro-bind: the list spans two platforms and
		// several optional toolchains, and bwrap aborts the whole spawn on a
		// single missing source. A root that does not exist here contributes
		// nothing to read, so skipping it is safe; failing is not.
		for _, root := range cfg.sandbox.readRoots {
			wrapped = append(wrapped, "--ro-bind-try", root, root)
		}
	}
	wrapped = append(wrapped,
		"--dev", "/dev",
		"--proc", "/proc",
	)
	roots := dedupeRoots(
		cfg.sandbox.worktree,
		cfg.sandbox.sandboxHome,
		cfg.sandbox.sidecarDir,
		cfg.sandbox.tmp,
		cfg.sandbox.sharedCache,
		cfg.sandbox.claudeState,
		cfg.sandbox.codexState,
		cfg.sandbox.copilotState,
		cfg.sandbox.opencodeState,
		cfg.sandbox.toolCache,
		cfg.sandbox.appSupport,
		cfg.sandbox.claudeScratch,
	)
	roots = dedupeRoots(append(roots, cfg.sandbox.gitShared...)...)
	for _, root := range roots {
		wrapped = append(wrapped, "--bind", root, root)
	}
	for _, root := range dedupeRoots(cfg.sandbox.gitReadonly...) {
		wrapped = append(wrapped, "--ro-bind", root, root)
	}
	if cfg.sandbox.gitWorktrees != "" {
		wrapped = append(wrapped, "--ro-bind", cfg.sandbox.gitWorktrees, cfg.sandbox.gitWorktrees)
	}
	if cfg.sandbox.gitAdminDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitAdminDir, cfg.sandbox.gitAdminDir)
	}
	if cfg.sandbox.gitOverlayRefDir != "" && cfg.sandbox.gitBranchRefDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayRefDir, cfg.sandbox.gitBranchRefDir)
	}
	if cfg.sandbox.gitOverlayLogDir != "" && cfg.sandbox.gitBranchLogDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayLogDir, cfg.sandbox.gitBranchLogDir)
	}
	if cfg.sandbox.gitOverlayRemoteRefDir != "" && cfg.sandbox.gitRemoteRefDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayRemoteRefDir, cfg.sandbox.gitRemoteRefDir)
	}
	if cfg.sandbox.gitOverlayRemoteLogDir != "" && cfg.sandbox.gitRemoteLogDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayRemoteLogDir, cfg.sandbox.gitRemoteLogDir)
	}
	if cfg.sandbox.gitOverlayTagRefDir != "" && cfg.sandbox.gitTagRefDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayTagRefDir, cfg.sandbox.gitTagRefDir)
	}
	if cfg.sandbox.gitOverlayTagLogDir != "" && cfg.sandbox.gitTagLogDir != "" {
		wrapped = append(wrapped, "--bind", cfg.sandbox.gitOverlayTagLogDir, cfg.sandbox.gitTagLogDir)
	}
	// The object overlay remains writable for loose objects, but explicit
	// maintenance must not create packs or commit-graph metadata that the
	// post-run publisher could copy into the shared clone.
	if cfg.sandbox.gitOverlayObjectDir != "" {
		for _, dir := range []string{"pack", "info"} {
			path := filepath.Join(cfg.sandbox.gitOverlayObjectDir, dir)
			wrapped = append(wrapped, "--ro-bind", path, path)
		}
	}
	// Re-lock the durable-config paths after every writable bind above:
	// bwrap resolves overlapping entries in argument order, so these must
	// come last to win over the state dir that contains them.
	for _, p := range dedupeRoots(cfg.sandbox.stateDenied...) {
		wrapped = append(wrapped, "--ro-bind", p, p)
	}
	// Re-lock readOnlyDir last: bwrap resolves overlapping bind entries in
	// argument order, so this must come after every writable --bind above to
	// win even when readOnlyDir sits inside a writable root (e.g. nested
	// under tmp — see injectReadOnlyProcessSandbox).
	if cfg.sandbox.readOnlyDir != "" {
		wrapped = append(wrapped, "--ro-bind", cfg.sandbox.readOnlyDir, cfg.sandbox.readOnlyDir)
	}
	wrapped = append(wrapped, "--", name)
	wrapped = append(wrapped, args...)
	// Detached verification clones have no branch ref to publish, but commits
	// still write objects into the overlay while advancing their private HEAD.
	// Always publish verified loose objects before the next command resets it.
	if cfg.sandbox.gitOverlayObjectDir != "" && cfg.sandbox.worktree != "" {
		return sandboxSyncShell(wrapped, cfg)
	}
	return bwrapPath, wrapped
}

func sandboxSyncShell(bwrapArgs []string, cfg *RunConfig) (wrappedName string, wrappedArgs []string) {
	script := strings.Join([]string{
		`sync_git_objects() {`,
		`  src=$1`,
		`  dst=$2`,
		`  [ -d "$src" ] || return 0`,
		`  mkdir -p "$dst" || return $?`,
		`  for hexdir in "$src"/[0-9a-f][0-9a-f]; do`,
		`    [ -d "$hexdir" ] || continue`,
		`    base=$(basename "$hexdir")`,
		`    mkdir -p "$dst/$base" || return $?`,
		`    for obj in "$hexdir"/*; do`,
		`      [ -f "$obj" ] || continue`,
		`      cp -p "$obj" "$dst/$base/" || return $?`,
		`    done`,
		`  done`,
		`}`,
		// Snapshot the overlay's starting point, not the durable ref after the
		// command. If another process advances the shared branch while the
		// sandbox runs, an unchanged overlay must not rewind it; a changed
		// overlay publishes only when the durable ref still matches this base.
		`base_ref=$(cat ` + shellQuote(cfg.sandbox.gitOverlayRefFile) + ` 2>/dev/null || true)`,
		`"$@"`,
		`status=$?`,
		`sync_status=0`,
		`if ! sync_git_objects ` + shellQuote(cfg.sandbox.gitOverlayObjectDir) + ` ` + shellQuote(cfg.sandbox.gitObjectDir) + `; then sync_status=$?; fi`,
		`new_ref=$(cat ` + shellQuote(cfg.sandbox.gitOverlayRefFile) + ` 2>/dev/null || true)`,
		`if [ "$sync_status" -eq 0 ] && [ -n "$new_ref" ] && git -C ` + shellQuote(cfg.sandbox.worktree) + ` cat-file -e "$new_ref^{commit}" 2>/dev/null; then`,
		`  if [ -z "$base_ref" ]; then`,
		`    sync_status=1`,
		`  elif [ "$new_ref" != "$base_ref" ]; then`,
		`    git -C ` + shellQuote(cfg.sandbox.worktree) + ` update-ref ` + shellQuote(cfg.sandbox.gitBranchRef) + ` "$new_ref" "$base_ref" || sync_status=$?`,
		`  fi`,
		`fi`,
		`if [ "$status" -eq 0 ] && [ "$sync_status" -ne 0 ]; then status=$sync_status; fi`,
		`exit "$status"`,
	}, "\n")
	args := append([]string{"-c", script, "sybra-sandbox-sync", bwrapPath}, bwrapArgs...)
	return "/bin/sh", args
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// buildReadProfile is a no-op on Linux: bwrap expresses the read allowlist as
// mount arguments (see wrapInvocation) rather than a profile file, so there is
// nothing to materialize.
func buildReadProfile(base string, _ []string, _ string) (string, error) {
	return base, nil
}
