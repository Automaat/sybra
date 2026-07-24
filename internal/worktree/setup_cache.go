package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// setupCacheMarkerName is git-excluded (never committed) — it records the
// content hash of the setup commands + lockfiles from the last setup run
// that completed successfully in this worktree. Every worktree reuse
// (restart churn, fix/review/conflict worktree reuse) otherwise re-runs the
// full setup-command list unconditionally; a reused worktree whose hash
// still matches skips re-running it entirely (issue #2505).
const setupCacheMarkerName = ".sybra-setup-cache"

// setupCacheLockfiles lists the lockfile names hashed into the setup cache
// key alongside the resolved command list, covering the toolchains Sybra
// projects commonly bootstrap with. A present file's content is hashed; an
// absent one contributes nothing, so a project using none of these is keyed
// on its setup commands alone.
var setupCacheLockfiles = []string{
	"go.sum", "go.mod",
	"package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
	"Cargo.lock", "poetry.lock", "Pipfile.lock", "uv.lock",
	"Gemfile.lock", "composer.lock",
	"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml", "mise.lock",
}

// setupCacheKey hashes the resolved setup commands together with the
// content of any lockfiles present at the worktree root, so a cache hit
// means "the exact bootstrap this worktree would run is unchanged" — it
// catches both a `.sybra.yaml` setup-block edit (which changes commands)
// and a dependency bump (which changes a lockfile's content without the
// command list itself changing at all).
func setupCacheKey(wtPath string, commands []string) string {
	h := sha256.New()
	for _, c := range commands {
		h.Write([]byte(c))
		h.Write([]byte{0})
	}
	for _, name := range setupCacheLockfiles {
		data, err := os.ReadFile(filepath.Join(wtPath, name))
		if err != nil {
			continue
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readSetupCacheKey returns the hash recorded by the last setup run that
// completed successfully in wtPath, if any.
func readSetupCacheKey(wtPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(wtPath, setupCacheMarkerName))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// writeSetupCacheKey records key as the hash of the setup run that just
// completed successfully, so the next reuse can compare against it. A
// failed prior run never calls this (the caller only reaches it after every
// command in the batch has succeeded), so a stale marker left over from an
// earlier successful run keeps forcing a re-run until one actually
// completes at the current key — never recording a hash for a failure.
//
// Best-effort: a failed write only costs a future cache hit, never
// worktree creation, so it is logged and swallowed rather than returned.
func (m *Manager) writeSetupCacheKey(ctx context.Context, taskID, wtPath, key string) {
	if err := addToInfoExclude(ctx, wtPath, setupCacheMarkerName); err != nil {
		m.logger.Warn("worktree.setup-cache-exclude", "task_id", taskID, "path", wtPath, "err", err)
	}
	path := filepath.Join(wtPath, setupCacheMarkerName)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		m.logger.Warn("worktree.setup-cache-write", "task_id", taskID, "path", wtPath, "err", err)
	}
}
