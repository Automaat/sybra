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
	branchRef      string
	branchRefDir   string
	branchLogDir   string
	sharedWritable []string
	sharedReadonly []string
}

type gitSharedPaths struct {
	writable     []string
	readonly     []string
	branchRef    string
	branchRefDir string
	branchLogDir string
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
	sharedPaths, err := resolveGitSharedWritablePaths(ctx, worktree)
	if err != nil {
		return gitSandboxRoots{}, err
	}
	roots.branchRef = sharedPaths.branchRef
	roots.branchRefDir = sharedPaths.branchRefDir
	roots.branchLogDir = sharedPaths.branchLogDir
	roots.sharedWritable = sharedPaths.writable
	roots.sharedReadonly = sharedPaths.readonly
	return roots, nil
}

func resolveGitSharedWritablePaths(ctx context.Context, worktree string) (gitSharedPaths, error) {
	var paths gitSharedPaths
	branchRef, err := gitSymbolicRef(ctx, worktree)
	if err != nil {
		return gitSharedPaths{}, fmt.Errorf("resolve current branch ref: %w", err)
	}
	paths.branchRef = branchRef
	addExisting := func(label string, args ...string) error {
		path, err := gitPath(ctx, worktree, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		paths.writable = append(paths.writable, path)
		return nil
	}
	addDir := func(label string, rel string) error {
		path, err := ensureGitPathDir(ctx, worktree, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		paths.writable = append(paths.writable, path)
		return nil
	}
	for _, spec := range []struct {
		label string
		args  []string
	}{
		{label: "resolve git objects dir", args: []string{"--git-path", "objects"}},
	} {
		if err := addExisting(spec.label, spec.args...); err != nil {
			return gitSharedPaths{}, err
		}
	}
	branchRefDir, err := ensureGitPathDir(ctx, worktree, filepath.Dir(branchRef))
	if err != nil {
		return gitSharedPaths{}, fmt.Errorf("resolve current branch dir: %w", err)
	}
	paths.branchRefDir = branchRefDir

	branchLogDir, err := ensureGitPathDir(ctx, worktree, filepath.Dir(filepath.Join("logs", branchRef)))
	if err != nil {
		return gitSharedPaths{}, fmt.Errorf("resolve current branch log dir: %w", err)
	}
	paths.branchLogDir = branchLogDir
	if _, err := ensureGitPathFile(ctx, worktree, filepath.Join("logs", branchRef)); err != nil {
		return gitSharedPaths{}, fmt.Errorf("resolve current branch log: %w", err)
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
			return gitSharedPaths{}, err
		}
	}
	paths.writable = dedupeRoots(paths.writable...)
	paths.readonly = dedupeRoots(paths.readonly...)
	return paths, nil
}

type gitBranchOverlay struct {
	refDir  string
	logDir  string
	refFile string
}

func prepareGitBranchOverlay(ctx context.Context, worktree, sandboxHome string, roots gitSandboxRoots) (gitBranchOverlay, error) {
	if roots.branchRef == "" || roots.branchRefDir == "" || roots.branchLogDir == "" {
		return gitBranchOverlay{}, nil
	}
	head, err := gitHeadCommit(ctx, worktree)
	if err != nil {
		return gitBranchOverlay{}, err
	}
	base := filepath.Join(sandboxHome, ".sybra-git-overlay")
	if err := os.RemoveAll(base); err != nil {
		return gitBranchOverlay{}, fmt.Errorf("reset %s: %w", base, err)
	}
	refDir := filepath.Join(base, "refs")
	logDir := filepath.Join(base, "logs")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		return gitBranchOverlay{}, fmt.Errorf("mkdir %s: %w", refDir, err)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return gitBranchOverlay{}, fmt.Errorf("mkdir %s: %w", logDir, err)
	}
	refFile := filepath.Join(refDir, filepath.Base(roots.branchRef))
	if err := os.WriteFile(refFile, []byte(head+"\n"), 0o644); err != nil {
		return gitBranchOverlay{}, fmt.Errorf("write %s: %w", refFile, err)
	}
	logFile := filepath.Join(logDir, filepath.Base(roots.branchRef))
	if src, err := gitPathRaw(ctx, worktree, "--git-path", filepath.Join("logs", roots.branchRef)); err == nil {
		if data, readErr := os.ReadFile(src); readErr == nil {
			if writeErr := os.WriteFile(logFile, data, 0o644); writeErr != nil {
				return gitBranchOverlay{}, fmt.Errorf("write %s: %w", logFile, writeErr)
			}
		}
	}
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		if err := os.WriteFile(logFile, nil, 0o644); err != nil {
			return gitBranchOverlay{}, fmt.Errorf("write %s: %w", logFile, err)
		}
	} else if err != nil {
		return gitBranchOverlay{}, err
	}
	canonRefDir, err := canonicalizeRoot(refDir)
	if err != nil {
		return gitBranchOverlay{}, err
	}
	canonLogDir, err := canonicalizeRoot(logDir)
	if err != nil {
		return gitBranchOverlay{}, err
	}
	canonRefFile, err := canonicalizeRoot(refFile)
	if err != nil {
		return gitBranchOverlay{}, err
	}
	return gitBranchOverlay{refDir: canonRefDir, logDir: canonLogDir, refFile: canonRefFile}, nil
}

func gitHeadCommit(ctx context.Context, worktree string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("git rev-parse --verify HEAD: %w", err)
		}
		return "", fmt.Errorf("git rev-parse --verify HEAD: %w: %s", err, msg)
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return "", fmt.Errorf("git rev-parse --verify HEAD: empty ref")
	}
	return head, nil
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
