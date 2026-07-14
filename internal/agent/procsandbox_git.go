package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var errGitSandboxNotRepo = errors.New("sandbox git roots: worktree is not a git repository")

type gitSandboxRoots struct {
	adminDir     string
	commonDir    string
	worktreesDir string
}

func resolveGitSandboxRoots(ctx context.Context, worktree string) (gitSandboxRoots, error) {
	adminDir, err := gitRevParsePath(ctx, worktree, "--git-dir")
	if err != nil {
		if errors.Is(err, errGitSandboxNotRepo) {
			if hasGitMetadataSentinel(worktree) {
				return gitSandboxRoots{}, fmt.Errorf("resolve git admin dir: %w", err)
			}
			return gitSandboxRoots{}, nil
		}
		return gitSandboxRoots{}, err
	}
	commonDir, err := gitRevParsePath(ctx, worktree, "--git-common-dir")
	if err != nil {
		return gitSandboxRoots{}, err
	}
	roots := gitSandboxRoots{
		adminDir:  adminDir,
		commonDir: commonDir,
	}
	worktreesDir, err := canonicalizeOptionalRoot(filepath.Join(commonDir, "worktrees"))
	if err != nil {
		return gitSandboxRoots{}, fmt.Errorf("resolve git worktrees dir: %w", err)
	}
	roots.worktreesDir = worktreesDir
	return roots, nil
}

func gitRevParsePath(ctx context.Context, worktree, arg string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", arg)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if looksLikeNotGitRepo(msg) {
			return "", errGitSandboxNotRepo
		}
		if msg == "" {
			return "", fmt.Errorf("git rev-parse %s: %w", arg, err)
		}
		return "", fmt.Errorf("git rev-parse %s: %w: %s", arg, err, msg)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("git rev-parse %s: empty path", arg)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	canon, err := canonicalizeRoot(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", arg, err)
	}
	return canon, nil
}

func canonicalizeOptionalRoot(root string) (string, error) {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", root)
	}
	return canonicalizeRoot(root)
}

func looksLikeNotGitRepo(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "this operation must be run in a work tree")
}

func hasGitMetadataSentinel(worktree string) bool {
	_, err := os.Stat(filepath.Join(worktree, ".git"))
	return err == nil
}
