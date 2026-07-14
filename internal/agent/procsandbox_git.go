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
	adminDir       string
	commonDir      string
	worktreesDir   string
	sharedWritable []string
	sharedReadonly []string
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
	sharedWritable, sharedReadonly, err := resolveGitSharedWritablePaths(ctx, worktree)
	if err != nil {
		return gitSandboxRoots{}, err
	}
	roots.sharedWritable = sharedWritable
	roots.sharedReadonly = sharedReadonly
	return roots, nil
}

func resolveGitSharedWritablePaths(ctx context.Context, worktree string) (shared, readonly []string, err error) {
	branchRef, err := gitSymbolicRef(ctx, worktree)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current branch ref: %w", err)
	}
	addExisting := func(label string, args ...string) error {
		path, err := gitPath(ctx, worktree, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		shared = append(shared, path)
		return nil
	}
	addDir := func(label string, rel string) error {
		path, err := ensureGitPathDir(ctx, worktree, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		shared = append(shared, path)
		return nil
	}
	addSiblingReadonly := func(label, dir, keep string) error {
		paths, err := siblingReadonlyEntries(dir, keep)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		readonly = append(readonly, paths...)
		return nil
	}

	for _, spec := range []struct {
		label string
		args  []string
	}{
		{label: "resolve git objects dir", args: []string{"--git-path", "objects"}},
	} {
		if err := addExisting(spec.label, spec.args...); err != nil {
			return nil, nil, err
		}
	}
	branchRefDir, err := ensureGitPathDir(ctx, worktree, filepath.Dir(branchRef))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current branch dir: %w", err)
	}
	shared = append(shared, branchRefDir)
	if err := addSiblingReadonly("resolve sibling branch refs", branchRefDir, filepath.Base(branchRef)); err != nil {
		return nil, nil, err
	}

	branchLogDir, err := ensureGitPathDir(ctx, worktree, filepath.Dir(filepath.Join("logs", branchRef)))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current branch log dir: %w", err)
	}
	shared = append(shared, branchLogDir)
	if err := addSiblingReadonly("resolve sibling branch logs", branchLogDir, filepath.Base(branchRef)); err != nil {
		return nil, nil, err
	}
	if _, err := ensureGitPathFile(ctx, worktree, filepath.Join("logs", branchRef)); err != nil {
		return nil, nil, fmt.Errorf("resolve current branch log: %w", err)
	}
	for _, spec := range []struct {
		label string
		rel   string
	}{
		{label: "resolve remote refs dir", rel: filepath.Join("refs", "remotes")},
		{label: "resolve remote logs dir", rel: filepath.Join("logs", "refs", "remotes")},
		{label: "resolve tag refs dir", rel: filepath.Join("refs", "tags")},
		{label: "resolve tag logs dir", rel: filepath.Join("logs", "refs", "tags")},
	} {
		if err := addDir(spec.label, spec.rel); err != nil {
			return nil, nil, err
		}
	}
	return dedupeRoots(shared...), dedupeRoots(readonly...), nil
}

func gitPath(ctx context.Context, worktree string, args ...string) (string, error) {
	path, err := gitPathRaw(ctx, worktree, args...)
	if err != nil {
		return "", err
	}
	canon, err := canonicalizeRoot(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize git %s: %w", strings.Join(append([]string{"rev-parse"}, args...), " "), err)
	}
	return canon, nil
}

func gitPathRaw(ctx context.Context, worktree string, args ...string) (string, error) {
	cmdArgs := append([]string{"rev-parse"}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if looksLikeNotGitRepo(msg) {
			return "", errGitSandboxNotRepo
		}
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(cmdArgs, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(cmdArgs, " "), err, msg)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("git %s: empty path", strings.Join(cmdArgs, " "))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	return path, nil
}

func gitRevParsePath(ctx context.Context, worktree, arg string) (string, error) {
	return gitPath(ctx, worktree, arg)
}

func gitSymbolicRef(ctx context.Context, worktree string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("git symbolic-ref -q HEAD: %w", err)
		}
		return "", fmt.Errorf("git symbolic-ref -q HEAD: %w: %s", err, msg)
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return "", fmt.Errorf("git symbolic-ref -q HEAD: empty ref")
	}
	return ref, nil
}

func ensureGitPathDir(ctx context.Context, worktree, rel string) (string, error) {
	path, err := gitPathRaw(ctx, worktree, "--git-path", rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", path, err)
	}
	canon, err := canonicalizeRoot(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", path, err)
	}
	return canon, nil
}

func ensureGitPathFile(ctx context.Context, worktree, rel string) (string, error) {
	path, err := gitPathRaw(ctx, worktree, "--git-path", rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, createErr := os.OpenFile(path, os.O_CREATE, 0o644)
		if createErr != nil {
			return "", fmt.Errorf("create %s: %w", path, createErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return "", fmt.Errorf("close %s: %w", path, closeErr)
		}
	} else if err != nil {
		return "", err
	}
	canon, err := canonicalizeRoot(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", path, err)
	}
	return canon, nil
}

func siblingReadonlyEntries(dir, keepBase string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == keepBase {
			continue
		}
		canon, err := canonicalizeRoot(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		paths = append(paths, canon)
	}
	return paths, nil
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
