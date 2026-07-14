package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	remoteRefDir   string
	remoteLogDir   string
	tagRefDir      string
	tagLogDir      string
	sharedWritable []string
	sharedReadonly []string
}

type gitSharedPaths struct {
	writable     []string
	readonly     []string
	branchRef    string
	branchRefDir string
	branchLogDir string
	remoteRefDir string
	remoteLogDir string
	tagRefDir    string
	tagLogDir    string
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
	roots.remoteRefDir = sharedPaths.remoteRefDir
	roots.remoteLogDir = sharedPaths.remoteLogDir
	roots.tagRefDir = sharedPaths.tagRefDir
	roots.tagLogDir = sharedPaths.tagLogDir
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
	resolveDir := func(label string, rel string) (string, error) {
		path, err := ensureGitPathDir(ctx, worktree, rel)
		if err != nil {
			return "", fmt.Errorf("%s: %w", label, err)
		}
		return path, nil
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
		dst   *string
	}{
		{label: "resolve remote refs dir", rel: filepath.Join("refs", "remotes"), dst: &paths.remoteRefDir},
		{label: "resolve remote logs dir", rel: filepath.Join("logs", "refs", "remotes"), dst: &paths.remoteLogDir},
		{label: "resolve tag refs dir", rel: filepath.Join("refs", "tags"), dst: &paths.tagRefDir},
		{label: "resolve tag logs dir", rel: filepath.Join("logs", "refs", "tags"), dst: &paths.tagLogDir},
	} {
		path, err := resolveDir(spec.label, spec.rel)
		if err != nil {
			return gitSharedPaths{}, err
		}
		*spec.dst = path
	}
	paths.writable = dedupeRoots(paths.writable...)
	paths.readonly = dedupeRoots(paths.readonly...)
	return paths, nil
}

type gitSandboxOverlay struct {
	branchRefDir  string
	branchLogDir  string
	branchRefFile string
	remoteRefDir  string
	remoteLogDir  string
	tagRefDir     string
	tagLogDir     string
}

func prepareGitSandboxOverlay(ctx context.Context, worktree, sandboxHome string, roots gitSandboxRoots) (gitSandboxOverlay, error) {
	if roots.branchRef == "" || roots.branchRefDir == "" || roots.branchLogDir == "" {
		return gitSandboxOverlay{}, nil
	}
	head, err := gitHeadCommit(ctx, worktree)
	if err != nil {
		return gitSandboxOverlay{}, err
	}
	base := filepath.Join(sandboxHome, ".sybra-git-overlay")
	if err := os.RemoveAll(base); err != nil {
		return gitSandboxOverlay{}, fmt.Errorf("reset %s: %w", base, err)
	}
	overlay := gitSandboxOverlay{}
	if overlay.branchRefDir, err = seedGitOverlayDir(base, "branch-refs", roots.branchRefDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	if overlay.branchLogDir, err = seedGitOverlayDir(base, "branch-logs", roots.branchLogDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	overlay.branchRefFile = filepath.Join(overlay.branchRefDir, filepath.Base(roots.branchRef))
	if err := os.WriteFile(overlay.branchRefFile, []byte(head+"\n"), 0o644); err != nil {
		return gitSandboxOverlay{}, fmt.Errorf("write %s: %w", overlay.branchRefFile, err)
	}
	logFile := filepath.Join(overlay.branchLogDir, filepath.Base(roots.branchRef))
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		if err := os.WriteFile(logFile, nil, 0o644); err != nil {
			return gitSandboxOverlay{}, fmt.Errorf("write %s: %w", logFile, err)
		}
	} else if err != nil {
		return gitSandboxOverlay{}, err
	}
	if overlay.remoteRefDir, err = seedGitOverlayDir(base, "remote-refs", roots.remoteRefDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	if overlay.remoteLogDir, err = seedGitOverlayDir(base, "remote-logs", roots.remoteLogDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	if overlay.tagRefDir, err = seedGitOverlayDir(base, "tag-refs", roots.tagRefDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	if overlay.tagLogDir, err = seedGitOverlayDir(base, "tag-logs", roots.tagLogDir); err != nil {
		return gitSandboxOverlay{}, err
	}
	canonRefFile, err := canonicalizeRoot(overlay.branchRefFile)
	if err != nil {
		return gitSandboxOverlay{}, err
	}
	overlay.branchRefFile = canonRefFile
	return overlay, nil
}

func seedGitOverlayDir(base, name, src string) (string, error) {
	dst := filepath.Join(base, name)
	if err := copyGitOverlayTree(src, dst); err != nil {
		return "", fmt.Errorf("seed %s overlay from %s: %w", name, src, err)
	}
	canon, err := canonicalizeRoot(dst)
	if err != nil {
		return "", err
	}
	return canon, nil
}

func copyGitOverlayTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", src)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsupported symlinked git metadata entry %s", path)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %s", path)
		}
		return copyGitOverlayFile(path, target, info.Mode().Perm())
	})
}

func copyGitOverlayFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
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
