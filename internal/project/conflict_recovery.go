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

// ResolvedUnmergedPaths reports every previously-conflicted path in an
// in-progress merge that is safe to auto-stage and commit: the working tree
// contains a marker-free resolution for it, whether or not `git add` was
// ever run. This covers both an agent that resolved a conflict on disk but
// never staged it (path still listed by `git ls-files -u`, the "resolved but
// unstaged" case) and one that staged the resolution but never committed
// (path staged, index already clean of unmerged stages). Returns nil when
// the tree has no merge to finish, when any conflicted path is binary
// (binary conflicts leave one side's content in place with no markers to
// verify against, so content inspection alone can't confirm a real
// resolution), or when any conflicted path still contains conflict markers.
func ResolvedUnmergedPaths(ctx context.Context, wtPath string) ([]string, error) {
	inMerge, err := mergeInProgress(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	if !inMerge {
		return nil, nil
	}

	unstaged, err := unmergedIndexPaths(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	var resolved []string
	stillUnmerged := make(map[string]struct{}, len(unstaged))
	for _, path := range unstaged {
		if !isSafeProtectedPath(path) {
			return nil, fmt.Errorf("unsafe merge path %q", path)
		}
		stillUnmerged[path] = struct{}{}
		data, readErr := os.ReadFile(filepath.Join(wtPath, filepath.FromSlash(path)))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				// A conflict "resolved" by deleting the working-tree file still
				// needs an explicit git rm/git add decision; don't guess intent.
				return nil, nil
			}
			return nil, fmt.Errorf("read unmerged path %s: %w", path, readErr)
		}
		if isBinaryContent(data) || hasConflictMarker(data) {
			return nil, nil
		}
		resolved = append(resolved, path)
	}

	// `git diff --cached --name-only` also reports still-unmerged paths (their
	// index entries differ from HEAD), so skip anything the loop above already
	// classified to avoid double-reporting the same path.
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return nil, fmt.Errorf("git diff --cached --name-only -z: %w", err)
		}
		return nil, fmt.Errorf("git diff --cached --name-only -z: %w: %s", err, detail)
	}

	for raw := range bytes.SplitSeq(out, []byte{0}) {
		path := string(raw)
		if path == "" {
			continue
		}
		if _, ok := stillUnmerged[path]; ok {
			continue
		}
		if !isSafeProtectedPath(path) {
			return nil, fmt.Errorf("unsafe merge path %q", path)
		}
		data, readErr := os.ReadFile(filepath.Join(wtPath, filepath.FromSlash(path)))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return nil, fmt.Errorf("read unmerged path %s: %w", path, readErr)
		}
		if hasConflictMarker(data) {
			return nil, nil
		}
		resolved = append(resolved, path)
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	return resolved, nil
}

// unmergedIndexPaths returns the unique set of paths that still have one or
// more unresolved merge stages in the index (git status "UU"/"AA"/"DU"/etc.),
// in the order git reports them. `git ls-files -u` emits one line per stage,
// so a path with all three stages present would otherwise be reported three
// times.
func unmergedIndexPaths(ctx context.Context, wtPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-u", "-z")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return nil, fmt.Errorf("git ls-files -u -z: %w", err)
		}
		return nil, fmt.Errorf("git ls-files -u -z: %w: %s", err, detail)
	}

	seen := make(map[string]struct{})
	var paths []string
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		tab := bytes.IndexByte(entry, '\t')
		if tab < 0 || tab+1 >= len(entry) {
			return nil, fmt.Errorf("parse git ls-files -u -z entry %q", string(entry))
		}
		path := string(entry[tab+1:])
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

func mergeInProgress(ctx context.Context, wtPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "MERGE_HEAD")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return false, fmt.Errorf("git rev-parse --git-path MERGE_HEAD: %w", err)
		}
		return false, fmt.Errorf("git rev-parse --git-path MERGE_HEAD: %w: %s", err, detail)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return false, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(wtPath, path)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return true, nil
	} else if os.IsNotExist(statErr) {
		return false, nil
	} else {
		return false, fmt.Errorf("stat MERGE_HEAD: %w", statErr)
	}
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

// isBinaryContent reports whether data looks binary using git's own
// heuristic: presence of a NUL byte. A binary merge conflict leaves the
// working tree at one side's raw content with no markers inserted, so
// hasConflictMarker alone can't tell "resolved" from "still one side's
// unmerged version" for binary paths.
func isBinaryContent(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}
