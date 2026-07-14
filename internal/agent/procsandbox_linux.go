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
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
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
		`  mkdir -p "$dst" "$dst/pack" || return $?`,
		`  for hexdir in "$src"/[0-9a-f][0-9a-f]; do`,
		`    [ -d "$hexdir" ] || continue`,
		`    base=$(basename "$hexdir")`,
		`    mkdir -p "$dst/$base" || return $?`,
		`    for obj in "$hexdir"/*; do`,
		`      [ -f "$obj" ] || continue`,
		`      cp -p "$obj" "$dst/$base/" || return $?`,
		`    done`,
		`  done`,
		`  for pack in "$src"/pack/pack-*; do`,
		`    [ -f "$pack" ] || continue`,
		`    cp -p "$pack" "$dst/pack/" || return $?`,
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

func dedupeRoots(roots ...string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
