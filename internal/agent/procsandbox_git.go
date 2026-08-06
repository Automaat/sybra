package agent

import (
	"bufio"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's repository object format may be SHA-1.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/gitexec"
)

var errGitSandboxNotRepo = errors.New("sandbox git roots: worktree is not a git repository")

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

type gitSandboxRoots struct {
	adminDir       string
	commonDir      string
	worktreesDir   string
	objectDir      string
	branchRef      string
	branchRefDir   string
	branchLogDir   string
	remoteRefDir   string
	remoteLogDir   string
	tagRefDir      string
	tagLogDir      string
	notesRefDir    string
	notesLogDir    string
	sharedWritable []string
	sharedReadonly []string
}

type gitSharedPaths struct {
	readonly     []string
	objectDir    string
	branchRef    string
	branchRefDir string
	branchLogDir string
	remoteRefDir string
	remoteLogDir string
	tagRefDir    string
	tagLogDir    string
	notesRefDir  string
	notesLogDir  string
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
	roots.objectDir = sharedPaths.objectDir
	roots.branchRef = sharedPaths.branchRef
	roots.branchRefDir = sharedPaths.branchRefDir
	roots.branchLogDir = sharedPaths.branchLogDir
	roots.remoteRefDir = sharedPaths.remoteRefDir
	roots.remoteLogDir = sharedPaths.remoteLogDir
	roots.tagRefDir = sharedPaths.tagRefDir
	roots.tagLogDir = sharedPaths.tagLogDir
	roots.notesRefDir = sharedPaths.notesRefDir
	roots.notesLogDir = sharedPaths.notesLogDir
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
	addReadonlyExisting := func(label string, args ...string) error {
		path, err := gitPath(ctx, worktree, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		paths.readonly = append(paths.readonly, path)
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
		if err := addReadonlyExisting(spec.label, spec.args...); err != nil {
			return gitSharedPaths{}, err
		}
		if spec.label == "resolve git objects dir" {
			paths.objectDir = paths.readonly[len(paths.readonly)-1]
		}
	}
	// Detached HEAD (branchRef == "") has no branch ref or reflog to make
	// writable, so there is nothing to resolve. prepareGitSandboxOverlay
	// already skips its branch-overlay work on an empty branchRef, so the
	// spec stays coherent — the run simply gets no branch-scoped grant.
	if branchRef != "" {
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
		// refs/notes/* (git notes add, default ref "commits") is annotation
		// data layered onto commits repo-wide — shared/idempotent truth like
		// remotes and tags, not a task's own exclusive work.
		{label: "resolve notes refs dir", rel: filepath.Join("refs", "notes"), dst: &paths.notesRefDir},
		{label: "resolve notes logs dir", rel: filepath.Join("logs", "refs", "notes"), dst: &paths.notesLogDir},
	} {
		path, err := resolveDir(spec.label, spec.rel)
		if err != nil {
			return gitSharedPaths{}, err
		}
		*spec.dst = path
	}
	paths.readonly = dedupeRoots(paths.readonly...)
	return paths, nil
}

type gitSandboxOverlay struct {
	objectDir     string
	branchRefDir  string
	branchLogDir  string
	branchRefFile string
	remoteRefDir  string
	remoteLogDir  string
	tagRefDir     string
	tagLogDir     string
}

func prepareGitSandboxOverlay(ctx context.Context, worktree, sandboxHome string, roots gitSandboxRoots) (gitSandboxOverlay, error) {
	base := filepath.Join(sandboxHome, ".sybra-git-overlay")
	if !sandboxUsesGitObjectOverlay() {
		if err := prepareGitLooseObjectDirs(roots.objectDir); err != nil {
			return gitSandboxOverlay{}, err
		}
		legacyObjects := filepath.Join(base, "objects")
		if err := migrateLegacyGitObjectOverlay(ctx, worktree, legacyObjects, roots.objectDir); err != nil {
			return gitSandboxOverlay{}, fmt.Errorf("migrate legacy git object overlay %s: %w", legacyObjects, err)
		}
	}
	if err := os.RemoveAll(base); err != nil {
		return gitSandboxOverlay{}, fmt.Errorf("reset %s: %w", base, err)
	}
	overlay := gitSandboxOverlay{}
	if roots.objectDir != "" && sandboxUsesGitObjectOverlay() {
		var err error
		if overlay.objectDir, err = prepareGitObjectOverlay(base); err != nil {
			return gitSandboxOverlay{}, err
		}
	}
	if roots.branchRef == "" || roots.branchRefDir == "" || roots.branchLogDir == "" {
		return overlay, nil
	}
	head, err := gitHeadCommit(ctx, worktree)
	if err != nil {
		return gitSandboxOverlay{}, err
	}
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

// migrateLegacyGitObjectOverlay publishes loose objects left by the former
// Darwin object-overlay sandbox into the clone's durable object store. It is
// deliberately conservative: every source object must be a canonical loose
// object whose decompressed content hashes to its pathname, and an existing
// destination must independently pass the same check. Pack files, alternates,
// symlinks, and other payloads fail closed so os.RemoveAll cannot discard data
// whose meaning Sybra has not proved.
func migrateLegacyGitObjectOverlay(ctx context.Context, worktree, legacyObjects, sharedObjects string) error {
	populated, err := gitObjectOverlayPopulated(legacyObjects)
	if err != nil {
		return err
	}
	if !populated {
		return nil
	}
	if strings.TrimSpace(sharedObjects) == "" {
		return errors.New("shared Git object directory is empty")
	}

	err = filepath.WalkDir(legacyObjects, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == legacyObjects || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(legacyObjects, path)
		if err != nil {
			return err
		}
		if !isGitObjectPayloadPath(legacyObjects, path) {
			return nil // derived commit-graph and info/packs metadata
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported legacy Git object entry %s", rel)
		}
		oid, ok := canonicalLooseObjectID(rel)
		if !ok {
			return fmt.Errorf("unsupported legacy Git object payload %s", rel)
		}
		return publishVerifiedLooseObject(path, filepath.Join(sharedObjects, rel), oid)
	})
	if err != nil {
		return err
	}

	// A migrated commit is not useful if its tree or parents are still absent.
	// Prove the task's entire HEAD graph resolves with only the shared store
	// before the caller removes the legacy overlay.
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{
		Dir: worktree,
		Env: append(stripEnvKeys(os.Environ(), "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"),
			"GIT_OBJECT_DIRECTORY="+sharedObjects,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		),
	}, "rev-list", "--objects", "--missing=print", "HEAD")
	if err != nil {
		return fmt.Errorf("verify migrated HEAD reachability: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if missing, ok := strings.CutPrefix(line, "?"); ok {
			return fmt.Errorf("verify migrated HEAD reachability: missing object %s", missing)
		}
	}
	return nil
}

func canonicalLooseObjectID(rel string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || len(parts[0]) != 2 || (len(parts[1]) != 38 && len(parts[1]) != 62) {
		return "", false
	}
	oid := parts[0] + parts[1]
	if oid != strings.ToLower(oid) {
		return "", false
	}
	_, err := hex.DecodeString(oid)
	return oid, err == nil
}

func publishVerifiedLooseObject(src, dst, oid string) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open legacy object %s: %w", oid, err)
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat legacy object %s: %w", oid, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("legacy object %s is not a regular file", oid)
	}
	if err := verifyLooseObject(source, oid); err != nil {
		return fmt.Errorf("verify legacy object %s: %w", oid, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("prepare shared object fanout for %s: %w", oid, err)
	}
	if existing, err := os.Open(dst); err == nil {
		defer func() { _ = existing.Close() }()
		if err := verifyLooseObject(existing, oid); err != nil {
			return fmt.Errorf("verify existing shared object %s: %w", oid, err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing shared object %s: %w", oid, err)
	}

	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind legacy object %s: %w", oid, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "tmp_obj_migrate_")
	if err != nil {
		return fmt.Errorf("stage shared object %s: %w", oid, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, source); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy shared object %s: %w", oid, err)
	}
	if err := verifyLooseObject(tmp, oid); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("verify staged shared object %s: %w", oid, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync shared object %s: %w", oid, err)
	}
	if err := tmp.Chmod(0o444); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod shared object %s: %w", oid, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close shared object %s: %w", oid, err)
	}
	if err := os.Link(tmpName, dst); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("publish shared object %s: %w", oid, err)
		}
		existing, openErr := os.Open(dst)
		if openErr != nil {
			return fmt.Errorf("open concurrently published shared object %s: %w", oid, openErr)
		}
		defer func() { _ = existing.Close() }()
		if verifyErr := verifyLooseObject(existing, oid); verifyErr != nil {
			return fmt.Errorf("verify concurrently published shared object %s: %w", oid, verifyErr)
		}
	}
	return nil
}

// GitHub rejects ordinary Git blobs above 100 MiB. Leave ample headroom for
// non-GitHub repositories while bounding work on a provider-controlled legacy
// overlay: validation runs in the trusted Sybra process, outside the provider
// sandbox, so it must not stream an arbitrarily large zlib payload.
const maxLegacyLooseObjectSize = 1 << 30
const maxLegacyLooseObjectDiskSize = maxLegacyLooseObjectSize + 1<<20

func verifyLooseObject(file *os.File, oid string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat compressed object: %w", err)
	}
	if info.Size() > maxLegacyLooseObjectDiskSize {
		return fmt.Errorf("compressed object size %d exceeds migration limit %d", info.Size(), maxLegacyLooseObjectDiskSize)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	compressed := bufio.NewReader(file)
	zr, err := zlib.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open zlib stream: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var digest hash.Hash
	switch len(oid) {
	case sha1.Size * 2:
		digest = sha1.New() // #nosec G401 -- required to verify SHA-1 Git object IDs.
	case sha256.Size * 2:
		digest = sha256.New()
	default:
		return fmt.Errorf("unsupported object ID length %d", len(oid))
	}
	reader := bufio.NewReader(io.TeeReader(zr, digest))
	headerBytes := make([]byte, 0, 64)
	for len(headerBytes) <= 128 {
		char, err := reader.ReadByte()
		if err != nil {
			return fmt.Errorf("read object header: %w", err)
		}
		if char == 0 {
			break
		}
		headerBytes = append(headerBytes, char)
	}
	if len(headerBytes) > 128 {
		return errors.New("object header exceeds 128 bytes")
	}
	header := string(headerBytes)
	typeName, sizeText, ok := strings.Cut(header, " ")
	if !ok || (typeName != "blob" && typeName != "tree" && typeName != "commit" && typeName != "tag") {
		return fmt.Errorf("invalid object header %q", header)
	}
	wantSize, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || wantSize < 0 {
		return fmt.Errorf("invalid object size %q", sizeText)
	}
	if wantSize > maxLegacyLooseObjectSize {
		return fmt.Errorf("object size %d exceeds migration limit %d", wantSize, maxLegacyLooseObjectSize)
	}
	gotSize, err := io.CopyN(io.Discard, reader, wantSize+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read object content: %w", err)
	}
	if gotSize != wantSize {
		return fmt.Errorf("object size = %d, want %d", gotSize, wantSize)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != oid {
		return fmt.Errorf("object hash = %s, want %s", got, oid)
	}
	return nil
}

func prepareGitLooseObjectDirs(objectDir string) error {
	if objectDir == "" {
		return nil
	}
	for i := range 256 {
		path := filepath.Join(objectDir, fmt.Sprintf("%02x", i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("prepare loose-object fanout %s: %w", path, err)
		}
	}
	return nil
}

func gitObjectOverlayPopulated(path string) (bool, error) {
	populated := false
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if current == path && os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if current != path && !entry.IsDir() && isGitObjectPayloadPath(path, current) {
			populated = true
			return fs.SkipAll
		}
		return nil
	})
	return populated, err
}

func isGitObjectPayloadPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	first, rest, hasRest := strings.Cut(rel, string(filepath.Separator))
	if first == "pack" {
		return true
	}
	if first == "info" {
		if !hasRest {
			return false
		}
		return rest != "commit-graph" && rest != "packs" &&
			!strings.HasPrefix(rest, "commit-graphs"+string(filepath.Separator))
	}
	if len(first) != 2 {
		return false
	}
	for _, char := range first {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func prepareGitObjectOverlay(base string) (string, error) {
	dst := filepath.Join(base, "objects")
	for _, dir := range []string{dst, filepath.Join(dst, "info"), filepath.Join(dst, "pack")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	canon, err := canonicalizeRoot(dst)
	if err != nil {
		return "", err
	}
	return canon, nil
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
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{
		Dir: worktree,
		Env: gitSandboxDiscoveryEnv(),
	}, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
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
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{
		Dir: worktree,
		Env: gitSandboxDiscoveryEnv(),
	}, cmdArgs...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if looksLikeNotGitRepo(msg) {
			return "", errGitSandboxNotRepo
		}
		return "", err
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

// gitExitCode reports a git process exit code, or -1 when err is not an exit
// failure (spawn error, context cancellation).
func gitExitCode(err error) int {
	if code, ok := gitexec.ExitCode(err); ok {
		return code
	}
	return -1
}

func gitRevParsePath(ctx context.Context, worktree, arg string) (string, error) {
	return gitPath(ctx, worktree, arg)
}

// gitSymbolicRef returns the branch ref HEAD points at, or "" when HEAD is
// detached.
//
// A detached HEAD is a normal state here, not a failure: a review worktree
// checked out at a pull-request head has no branch. Treating it as an error
// failed the whole run closed under sandbox enforce with a bare
// "git symbolic-ref -q HEAD: exit status 1", which reads like a broken repo
// rather than "this checkout has no branch".
//
// `-q` is what makes the two distinguishable: it suppresses the
// "ref HEAD is not a symbolic ref" message, so a detached HEAD exits 1 with
// empty output, while a real failure (not a repository, unreadable HEAD)
// exits 128 and prints. Only the first is swallowed.
func gitSymbolicRef(ctx context.Context, worktree string) (string, error) {
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{
		Dir: worktree,
		Env: gitSandboxDiscoveryEnv(),
	}, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" && gitExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	// Exit 0 with no ref is not a state git produces; treat it as a real fault
	// rather than silently reporting "detached".
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return "", fmt.Errorf("git symbolic-ref -q HEAD: empty ref")
	}
	return ref, nil
}

// gitSandboxDiscoveryEnv prevents Sybra's own ambient Git object overrides
// from changing which repository paths are trusted and granted to an agent.
func gitSandboxDiscoveryEnv() []string {
	return stripEnvKeys(os.Environ(), "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES")
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
