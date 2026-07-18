package project

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var conflictMarkerPrefixes = [][]byte{
	[]byte("<<<<<<<"),
	[]byte("======="),
	[]byte(">>>>>>>"),
}

// ResolvedUnmergedPaths reports the worktree's currently-unmerged paths only
// when every one of them is already marker-free on disk. Returns nil when the
// tree has no unmerged paths or when any path still contains conflict markers,
// so callers can treat the marker-free case as a distinct, self-healable state.
func ResolvedUnmergedPaths(ctx context.Context, wtPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return nil, fmt.Errorf("git diff --name-only --diff-filter=U: %w", err)
		}
		return nil, fmt.Errorf("git diff --name-only --diff-filter=U: %w: %s", err, detail)
	}

	var paths []string
	for raw := range strings.SplitSeq(string(out), "\n") {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if !isSafeProtectedPath(path) {
			return nil, fmt.Errorf("unsafe unmerged path %q", path)
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, nil
	}

	for _, path := range paths {
		data, readErr := os.ReadFile(filepath.Join(wtPath, filepath.FromSlash(path)))
		if readErr != nil {
			return nil, fmt.Errorf("read unmerged path %s: %w", path, readErr)
		}
		if hasConflictMarker(data) {
			return nil, nil
		}
	}
	return paths, nil
}

func hasConflictMarker(data []byte) bool {
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		for _, prefix := range conflictMarkerPrefixes {
			if bytes.HasPrefix(line, prefix) {
				return true
			}
		}
	}
	return false
}
