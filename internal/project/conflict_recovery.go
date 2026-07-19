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

// ResolvedUnmergedPaths reports resolved paths in an in-progress merge. It
// accepts both already-staged resolutions and marker-free working-tree files
// that are still unmerged in Git's index, so the caller can stage/checkpoint
// agent fixes that forgot `git add`.
func ResolvedUnmergedPaths(ctx context.Context, wtPath string) ([]string, error) {
	inMerge, err := mergeInProgress(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	if !inMerge {
		return nil, nil
	}

	unmerged := exec.CommandContext(ctx, "git", "ls-files", "-u", "-z")
	unmerged.Dir = wtPath
	unmergedOut, err := unmerged.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(unmergedOut))
		if detail == "" {
			return nil, fmt.Errorf("git ls-files -u -z: %w", err)
		}
		return nil, fmt.Errorf("git ls-files -u -z: %w: %s", err, detail)
	}
	if len(unmergedOut) > 0 {
		paths, err := unmergedPathsFromLSFiles(unmergedOut)
		if err != nil {
			return nil, err
		}
		return markerFreeWorktreePaths(wtPath, paths, true)
	}

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

	var paths []string
	for raw := range bytes.SplitSeq(out, []byte{0}) {
		path := string(raw)
		if path == "" {
			continue
		}
		if !isSafeProtectedPath(path) {
			return nil, fmt.Errorf("unsafe merge path %q", path)
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, nil
	}

	return markerFreeWorktreePaths(wtPath, paths, false)
}

func unmergedPathsFromLSFiles(out []byte) ([]string, error) {
	seen := map[string]bool{}
	var paths []string
	for raw := range bytes.SplitSeq(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		_, pathBytes, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok || len(pathBytes) == 0 {
			return nil, fmt.Errorf("parse git ls-files -u entry %q", string(raw))
		}
		path := string(pathBytes)
		if !isSafeProtectedPath(path) {
			return nil, fmt.Errorf("unsafe merge path %q", path)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths, nil
}

func markerFreeWorktreePaths(wtPath string, paths []string, rejectBinary bool) ([]string, error) {
	for _, path := range paths {
		data, readErr := os.ReadFile(filepath.Join(wtPath, filepath.FromSlash(path)))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				if rejectBinary {
					return nil, nil
				}
				continue
			}
			return nil, fmt.Errorf("read unmerged path %s: %w", path, readErr)
		}
		if hasConflictMarker(data) || (rejectBinary && bytes.IndexByte(data, 0) >= 0) {
			return nil, nil
		}
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
