//go:build linux

package agent

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var (
	bwrapPathOnce sync.Once
	bwrapPath     string
)

func sandboxExecAvailable() bool {
	bwrapPathOnce.Do(func() {
		if p, err := exec.LookPath("bwrap"); err == nil {
			bwrapPath = p
		}
	})
	return bwrapPath != ""
}

func sandboxWrapperName() string { return "bwrap" }

// Linux bind-mounts branch refs into an overlay and publishes them only after
// sandboxSyncShell has copied the corresponding objects into the shared
// store, so object staging is safe and required on this platform.
func sandboxUsesGitObjectOverlay() bool { return true }

func materializeSandboxProfile() (string, error) {
	return "", nil
}

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
	if cfg.sandbox.gitOverlayRefFile != "" && cfg.sandbox.gitBranchRef != "" && cfg.sandbox.worktree != "" {
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
		`"$@"`,
		`status=$?`,
		`sync_status=0`,
		`if ! sync_git_objects ` + shellQuote(cfg.sandbox.gitOverlayObjectDir) + ` ` + shellQuote(cfg.sandbox.gitObjectDir) + `; then sync_status=$?; fi`,
		`new_ref=$(cat ` + shellQuote(cfg.sandbox.gitOverlayRefFile) + ` 2>/dev/null || true)`,
		`if [ "$sync_status" -eq 0 ] && [ -n "$new_ref" ] && git -C ` + shellQuote(cfg.sandbox.worktree) + ` cat-file -e "$new_ref^{commit}" 2>/dev/null; then`,
		`  git -C ` + shellQuote(cfg.sandbox.worktree) + ` update-ref ` + shellQuote(cfg.sandbox.gitBranchRef) + ` "$new_ref" || sync_status=$?`,
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
