// Package remotehandback validates staged daemon results and imports their Git
// outcome behind the leader-owned generation and base compare-and-swap fence.
package remotehandback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/gitexec"
)

var ErrStale = errors.New("remote handback: stale generation or canonical base")

// Guard re-reads leader-owned state. It is invoked before staging and again
// immediately before canonical mutation to close the generation/base TOCTOU
// window.
type Guard func(context.Context) (executioncontract.GenerationFence, string, error)

// Lock must serialize every mutation of the canonical worktree for the
// duration of validate-and-publish. A nil lock is rejected: optimistic checks
// alone cannot prevent a concurrent local completion from being reset away.
type Lock func(context.Context, string, func() error) error

// BeforePublish runs after complete isolated Git validation and the final
// leader guard, but before canonical Git mutation. It shares the caller's
// canonical locks and may atomically publish non-Git domain outputs.
type BeforePublish func([]executioncontract.ArtifactMember) error

// ImportGit applies committed, dirty, and untracked Git state only after the
// complete package has been materialized in an isolated staging clone. It
// returns declared non-Git artifacts for their leader-owned domain importers.
func ImportGit(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, content []byte, guard Guard, lock Lock) ([]executioncontract.ArtifactMember, error) {
	return ImportGitWithBeforePublish(ctx, target, spec, manifest, content, guard, lock, nil)
}

func ImportGitWithBeforePublish(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, content []byte, guard Guard, lock Lock, before BeforePublish) ([]executioncontract.ArtifactMember, error) {
	return importGitWithBeforePublish(ctx, target, spec, manifest, content, guard, lock, before, true)
}

// ValidateGitWithBeforePublish performs the same complete isolated package,
// ancestry, patch, and generation validation as ImportGit, and publishes
// declared non-Git outputs through before, but deliberately discards all Git
// mutations. Leader-owned verifier runs use it to preserve local disposable
// clone semantics when execution happened on a daemon.
func ValidateGitWithBeforePublish(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, content []byte, guard Guard, lock Lock, before BeforePublish) ([]executioncontract.ArtifactMember, error) {
	return importGitWithBeforePublish(ctx, target, spec, manifest, content, guard, lock, before, false)
}

func importGitWithBeforePublish(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, content []byte, guard Guard, lock Lock, before BeforePublish, publishGit bool) ([]executioncontract.ArtifactMember, error) {
	pkg, err := executioncontract.ValidateArtifactPackage(manifest, content)
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, errors.New("remote handback: leader generation guard is required")
	}
	if lock == nil {
		return nil, errors.New("remote handback: canonical worktree lock is required")
	}
	var imported []executioncontract.ArtifactMember
	err = lock(ctx, target, func() error {
		var importErr error
		imported, importErr = importGitLocked(ctx, target, spec, manifest, pkg, guard, before, publishGit)
		return importErr
	})
	return imported, err
}

//nolint:funlen // Validate, stage, recheck, publish, and rollback are one canonical mutation transaction.
func importGitLocked(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, pkg executioncontract.ArtifactPackage, guard Guard, before BeforePublish, publishGit bool) ([]executioncontract.ArtifactMember, error) {
	current, canonicalBase, err := guard(ctx)
	if err != nil {
		return nil, err
	}
	if current != spec.Fence || manifest.Fence != spec.Fence || manifest.Workspace.RepositoryID != spec.Workspace.RepositoryID ||
		manifest.Workspace.BaseSHA != spec.Workspace.BaseSHA || manifest.Workspace.BaseRef != spec.Workspace.BaseRef {
		return nil, ErrStale
	}
	if err := validateOutputs(spec, manifest); err != nil {
		return nil, err
	}
	stagedPatch, unstagedPatch, untracked, nonGit := partitionArtifacts(manifest, pkg)
	stagingParent := filepath.Dir(target)
	stagingPrefix := handbackScratchPrefix(target)
	if err := reclaimHandbackScratch(stagingParent, stagingPrefix, time.Now()); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(stagingParent, stagingPrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	checkout := filepath.Join(staging, "checkout")
	if err := gitexec.Run(ctx, gitexec.Options{}, "clone", "--no-checkout", "--no-local", "--", target, checkout); err != nil {
		return nil, err
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: checkout}, "checkout", "--detach", spec.Workspace.BaseSHA); err != nil {
		return nil, err
	}
	for i := range manifest.Artifacts {
		entry := &manifest.Artifacts[i]
		member := pkg.Members[i]
		if entry.Kind == "git_bundle" {
			bundle := filepath.Join(staging, "commits.bundle")
			if err := os.WriteFile(bundle, member.Content, 0o600); err != nil {
				return nil, err
			}
			if err := gitexec.Run(ctx, gitexec.Options{Dir: checkout}, "bundle", "verify", bundle); err != nil {
				return nil, fmt.Errorf("remote handback: invalid Git bundle: %w", err)
			}
			remoteRef := "refs/sybra/handback/" + spec.RunID
			if err := gitexec.Run(ctx, gitexec.Options{Dir: checkout}, "fetch", bundle, remoteRef+":refs/sybra/import/"+spec.RunID); err != nil {
				return nil, err
			}
		}
	}
	if _, err := gitexec.Output(ctx, gitexec.Options{Dir: checkout}, "rev-parse", "--verify", manifest.Workspace.FinalSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("remote handback: final commit missing: %w", err)
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: checkout}, "merge-base", "--is-ancestor", spec.Workspace.BaseSHA, manifest.Workspace.FinalSHA); err != nil {
		return nil, errors.New("remote handback: final commit does not descend from immutable base")
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: checkout}, "checkout", "--detach", manifest.Workspace.FinalSHA); err != nil {
		return nil, err
	}
	publicationStates := make(map[string]struct{}, 3+len(untracked))
	if err := recordPublicationState(ctx, checkout, publicationStates); err != nil {
		return nil, err
	}
	if err := applyPatch(ctx, checkout, stagedPatch, staging, true); err != nil {
		return nil, err
	}
	if err := recordPublicationState(ctx, checkout, publicationStates); err != nil {
		return nil, err
	}
	if err := applyPatch(ctx, checkout, unstagedPatch, staging, false); err != nil {
		return nil, err
	}
	if err := recordPublicationState(ctx, checkout, publicationStates); err != nil {
		return nil, err
	}
	for i, member := range untracked {
		entry := entryFor(manifest, member.Root, member.Path)
		if err := writeMember(checkout, staging, member, entry.Mode); err != nil {
			return nil, err
		}
		untracked[i] = member
		if err := recordPublicationState(ctx, checkout, publicationStates); err != nil {
			return nil, err
		}
	}
	if !publishGit {
		current, canonicalBase, err = guard(ctx)
		if err != nil {
			return nil, err
		}
		if current != spec.Fence || canonicalBase != spec.Workspace.BaseSHA {
			return nil, ErrStale
		}
		if before != nil {
			if err := before(nonGit); err != nil {
				return nil, err
			}
		}
		return nonGit, nil
	}
	// Only recognize a journaled publication after the bundle, ancestry,
	// patches, and untracked members have passed the same isolated validation
	// as a first import. This prevents a coincidentally matching canonical tree
	// from bypassing semantic package validation.
	published, err := matchesPublishedGit(ctx, target, manifest, stagedPatch, unstagedPatch, untracked)
	if err != nil {
		return nil, err
	}
	if published {
		current, canonicalBase, err = guard(ctx)
		if err != nil {
			return nil, err
		}
		if current != spec.Fence || canonicalBase != manifest.Workspace.FinalSHA {
			return nil, ErrStale
		}
		return nonGit, nil
	}
	recovering := false
	if canonicalBase == manifest.Workspace.FinalSHA {
		recovering, err = partialPublicationIsRepairable(ctx, target, publicationStates)
		if err != nil {
			return nil, err
		}
	}
	if canonicalBase != spec.Workspace.BaseSHA && !recovering {
		return nil, ErrStale
	}
	// Recheck leader state and the worktree compare-and-swap immediately before
	// canonical mutation.
	current, canonicalBase, err = guard(ctx)
	if err != nil {
		return nil, err
	}
	validRecoveryBase := recovering && canonicalBase == manifest.Workspace.FinalSHA
	if current != spec.Fence || (canonicalBase != spec.Workspace.BaseSHA && !validRecoveryBase) {
		return nil, ErrStale
	}
	if !recovering {
		if err := requireBaseAndClean(ctx, target, spec.Workspace.BaseSHA); err != nil {
			return nil, err
		}
	} else {
		if err := gitexec.Run(ctx, gitexec.Options{Dir: target}, "reset", "--hard", manifest.Workspace.FinalSHA); err != nil {
			return nil, err
		}
		// A crash after applying a staged addition leaves that path untracked
		// after reset. The exact checkpoint fingerprint above makes it safe to
		// remove all non-ignored publication residue before deterministic replay.
		if err := gitexec.Run(ctx, gitexec.Options{Dir: target}, "clean", "-fd", "--", "."); err != nil {
			return nil, err
		}
	}
	if before != nil {
		if err := before(nonGit); err != nil {
			return nil, err
		}
	}
	if manifest.Workspace.FinalSHA != spec.Workspace.BaseSHA {
		if err := gitexec.Run(ctx, gitexec.Options{Dir: target}, "fetch", checkout, manifest.Workspace.FinalSHA); err != nil {
			return nil, err
		}
	}
	rollback := func() {
		_ = gitexec.Run(context.WithoutCancel(ctx), gitexec.Options{Dir: target}, "reset", "--hard", spec.Workspace.BaseSHA)
		for _, member := range untracked {
			_ = os.Remove(filepath.Join(target, filepath.FromSlash(member.Path)))
		}
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: target}, "reset", "--hard", manifest.Workspace.FinalSHA); err != nil {
		return nil, err
	}
	if err := applyPatch(ctx, target, stagedPatch, staging, true); err != nil {
		rollback()
		return nil, err
	}
	if err := applyPatch(ctx, target, unstagedPatch, staging, false); err != nil {
		rollback()
		return nil, err
	}
	for _, member := range untracked {
		entry := entryFor(manifest, member.Root, member.Path)
		if err := writeMember(target, staging, member, entry.Mode); err != nil {
			rollback()
			return nil, err
		}
	}
	return nonGit, nil
}

func handbackScratchPrefix(target string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return ".sybra-handback-" + hex.EncodeToString(sum[:]) + "-"
}

func reclaimHandbackScratch(parent, targetPrefix string, now time.Time) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	legacyCutoff := now.Add(-24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		remove := strings.HasPrefix(name, targetPrefix)
		if !remove && isLegacyHandbackScratch(name) {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			remove = info.ModTime().Before(legacyCutoff)
		}
		if remove {
			if err := fsutil.RemoveAllForce(filepath.Join(parent, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func isLegacyHandbackScratch(name string) bool {
	suffix := strings.TrimPrefix(name, ".sybra-handback-")
	if suffix == name || suffix == "" {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func partialPublicationIsRepairable(ctx context.Context, target string, expected map[string]struct{}) (bool, error) {
	fingerprint, err := publicationStateFingerprint(ctx, target)
	if err != nil {
		return false, err
	}
	_, ok := expected[fingerprint]
	return ok, nil
}

func recordPublicationState(ctx context.Context, dir string, states map[string]struct{}) error {
	fingerprint, err := publicationStateFingerprint(ctx, dir)
	if err != nil {
		return err
	}
	states[fingerprint] = struct{}{}
	resetFingerprint, err := resetPublicationStateFingerprint(ctx, dir)
	if err != nil {
		return err
	}
	states[resetFingerprint] = struct{}{}
	return nil
}

// resetPublicationStateFingerprint records the only residue reset --hard can
// leave from a valid checkpoint: pre-existing untracked members. Index-added
// paths are removed by reset. This closes the crash window between reset and
// clean without accepting any state Sybra could not have produced.
func resetPublicationStateFingerprint(ctx context.Context, dir string) (string, error) {
	untracked, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: dir}, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return "", err
	}
	paths := make([]string, 0)
	for path := range strings.SplitSeq(strings.TrimSuffix(string(untracked), "\x00"), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	var status bytes.Buffer
	for _, path := range paths {
		status.WriteString("?? ")
		status.WriteString(path)
		status.WriteByte(0)
	}
	return publicationStateFingerprintForStatus(dir, status.Bytes())
}

// publicationStateFingerprint describes every state the canonical publisher
// can leave between its individual Git/file operations. Raw porcelain records
// preserve index/worktree status and exact path bytes; hashing each member's
// type, executable bit, and content prevents a concurrent edit with the same
// status shape from being mistaken for an interrupted publication.
func publicationStateFingerprint(ctx context.Context, dir string) (string, error) {
	status, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: dir}, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return "", err
	}
	return publicationStateFingerprintForStatus(dir, status)
}

func publicationStateFingerprintForStatus(dir string, status []byte) (string, error) {
	digest := sha256.New()
	writeFingerprintField(digest, status)
	for record := range strings.SplitSeq(strings.TrimSuffix(string(status), "\x00"), "\x00") {
		if record == "" {
			continue
		}
		path, parseErr := publicationStatusPath(record)
		if parseErr != nil {
			return "", errors.New("remote handback: malformed Git status record")
		}
		writeFingerprintField(digest, []byte(path))
		full := filepath.Join(dir, filepath.FromSlash(path))
		info, statErr := os.Lstat(full)
		if errors.Is(statErr, os.ErrNotExist) {
			writeFingerprintField(digest, []byte("missing"))
			continue
		}
		if statErr != nil {
			return "", statErr
		}
		mode := "regular"
		var content []byte
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			mode = "symlink"
			destination, readErr := os.Readlink(full)
			if readErr != nil {
				return "", readErr
			}
			content = []byte(destination)
		case info.Mode().IsRegular():
			if info.Mode().Perm()&0o111 != 0 {
				mode = "executable"
			}
			content, statErr = os.ReadFile(full)
			if statErr != nil {
				return "", statErr
			}
		default:
			return "", fmt.Errorf("remote handback: unsupported publication member type for %q", path)
		}
		writeFingerprintField(digest, []byte(mode))
		writeFingerprintField(digest, content)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func publicationStatusPath(record string) (string, error) {
	reader := strings.NewReader(record)
	if _, err := reader.Seek(3, io.SeekStart); err != nil || reader.Len() == 0 {
		return "", errors.New("invalid status record")
	}
	path, err := io.ReadAll(reader)
	return string(path), err
}

func writeFingerprintField(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}

func partitionArtifacts(manifest executioncontract.ArtifactManifest, pkg executioncontract.ArtifactPackage) (staged, unstaged []byte, untracked, nonGit []executioncontract.ArtifactMember) {
	for i := range manifest.Artifacts {
		entry := &manifest.Artifacts[i]
		member := pkg.Members[i]
		switch entry.Kind {
		case "git_bundle":
		case "git_staged_patch":
			staged = member.Content
		case "git_unstaged_patch":
			unstaged = member.Content
		case "git_untracked":
			untracked = append(untracked, member)
		default:
			nonGit = append(nonGit, member)
		}
	}
	return staged, unstaged, untracked, nonGit
}

// matchesPublishedGit recognizes the exact target state of a journaled import.
// It is intentionally stricter than checking HEAD: dirty/index state and every
// untracked byte/mode must also match, so an unrelated canonical edit can never
// be mistaken for crash recovery.
func matchesPublishedGit(ctx context.Context, target string, manifest executioncontract.ArtifactManifest, stagedPatch, unstagedPatch []byte, untracked []executioncontract.ArtifactMember) (bool, error) {
	head, err := gitexec.Output(ctx, gitexec.Options{Dir: target}, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return false, err
	}
	if head != manifest.Workspace.FinalSHA {
		return false, nil
	}
	actualStaged, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: target}, "diff", "--cached", "--binary", "--full-index", "HEAD", "--", ".")
	if err != nil {
		return false, err
	}
	actualUnstaged, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: target}, "diff", "--binary", "--full-index", "--", ".")
	if err != nil {
		return false, err
	}
	if !slices.Equal(actualStaged, stagedPatch) || !slices.Equal(actualUnstaged, unstagedPatch) {
		return false, nil
	}
	listed, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: target}, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return false, err
	}
	actualPaths := strings.Split(strings.TrimSuffix(string(listed), "\x00"), "\x00")
	if len(listed) == 0 {
		actualPaths = nil
	}
	expectedPaths := make([]string, 0, len(untracked))
	for _, member := range untracked {
		expectedPaths = append(expectedPaths, member.Path)
		entry := entryFor(manifest, member.Root, member.Path)
		full := filepath.Join(target, filepath.FromSlash(member.Path))
		info, statErr := os.Lstat(full)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return false, nil
			}
			return false, statErr
		}
		var content []byte
		if info.Mode()&os.ModeSymlink != 0 {
			destination, readErr := os.Readlink(full)
			if readErr != nil {
				return false, readErr
			}
			content = []byte(destination)
		} else {
			content, statErr = os.ReadFile(full)
			if statErr != nil {
				return false, statErr
			}
		}
		actualMode := uint32(0o100644)
		if info.Mode()&os.ModeSymlink != 0 {
			actualMode = 0o120000
		} else if info.Mode()&0o111 != 0 {
			actualMode = 0o100755
		}
		if !slices.Equal(content, member.Content) || actualMode != entry.Mode {
			return false, nil
		}
	}
	slices.Sort(expectedPaths)
	slices.Sort(actualPaths)
	return slices.Equal(actualPaths, expectedPaths), nil
}

func validateOutputs(spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest) error {
	entries := make(map[string]executioncontract.ArtifactEntry, len(manifest.Artifacts))
	declared := make(map[string]bool, len(spec.ExpectedOutputs))
	for _, expected := range spec.ExpectedOutputs {
		declared[expected.Name] = true
	}
	for i := range manifest.Artifacts {
		entry := &manifest.Artifacts[i]
		entries[entry.Name] = *entry
		if declared[entry.Name] ||
			(entry.Name == "git-bundle" && entry.Kind == "git_bundle" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/commits.bundle") ||
			(entry.Name == "git-staged-patch" && entry.Kind == "git_staged_patch" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/staged.patch") ||
			(entry.Name == "git-unstaged-patch" && entry.Kind == "git_unstaged_patch" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/unstaged.patch") ||
			(strings.HasPrefix(entry.Name, "untracked:") && entry.Kind == "git_untracked" && entry.Root == executioncontract.RootWorktree && entry.Name == "untracked:"+entry.Path) {
			continue
		}
		return fmt.Errorf("remote handback: output %q was not declared", entry.Name)
	}
	for _, expected := range spec.ExpectedOutputs {
		entry, ok := entries[expected.Name]
		if !ok {
			if expected.Required {
				return fmt.Errorf("remote handback: required output %q is missing", expected.Name)
			}
			continue
		}
		limit := expected.MaxBytes
		if limit == 0 {
			limit = executioncontract.MaxArtifactEntrySize
		}
		if entry.Kind != expected.Kind || entry.Root != expected.Root || entry.Path != expected.Path || entry.Sensitivity != expected.Sensitivity || entry.SizeBytes > limit ||
			(len(expected.MediaTypes) > 0 && !slices.Contains(expected.MediaTypes, entry.MediaType)) {
			return fmt.Errorf("remote handback: output %q differs from its declaration", expected.Name)
		}
	}
	return nil
}

func requireBaseAndClean(ctx context.Context, target, base string) error {
	head, err := gitexec.Output(ctx, gitexec.Options{Dir: target}, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head != base {
		return ErrStale
	}
	status, err := gitexec.Output(ctx, gitexec.Options{Dir: target}, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%w: canonical worktree is not clean", ErrStale)
	}
	return nil
}

func applyPatch(ctx context.Context, dir string, patch []byte, scratch string, index bool) error {
	if len(patch) == 0 {
		return nil
	}
	name := "unstaged.patch"
	if index {
		name = "staged.patch"
	}
	path := filepath.Join(scratch, name)
	if err := os.WriteFile(path, patch, 0o600); err != nil {
		return err
	}
	args := []string{"apply", "--binary"}
	if index {
		args = append(args, "--index")
	}
	return gitexec.Run(ctx, gitexec.Options{Dir: dir}, append(args, "--", path)...)
}

func entryFor(manifest executioncontract.ArtifactManifest, root executioncontract.LogicalRoot, path string) executioncontract.ArtifactEntry {
	for i := range manifest.Artifacts {
		entry := &manifest.Artifacts[i]
		if entry.Root == root && entry.Path == path {
			return *entry
		}
	}
	return executioncontract.ArtifactEntry{}
}

func writeMember(root, scratch string, member executioncontract.ArtifactMember, mode uint32) error {
	full := filepath.Join(root, filepath.FromSlash(member.Path))
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realRoot, realParent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("remote handback: member parent escapes worktree")
	}
	if _, err := os.Lstat(full); err == nil {
		return errors.New("remote handback: untracked member collides with existing path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mode == 0o120000 {
		return fsutil.AtomicSymlinkNewFromDir(full, scratch, string(member.Content))
	}
	perm := os.FileMode(0o600)
	if mode == 0o100755 {
		perm = 0o700
	}
	return fsutil.AtomicWriteNewModeFromDir(full, scratch, member.Content, perm)
}
