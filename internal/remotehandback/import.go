// Package remotehandback validates staged daemon results and imports their Git
// outcome behind the leader-owned generation and base compare-and-swap fence.
package remotehandback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
)

var ErrStale = errors.New("remote handback: stale generation or canonical base")

// Guard re-reads leader-owned state. It is invoked before staging and again
// immediately before canonical mutation to close the generation/base TOCTOU
// window.
type Guard func(context.Context) (executioncontract.GenerationFence, string, error)

// ImportGit applies committed, dirty, and untracked Git state only after the
// complete package has been materialized in an isolated staging clone. It
// returns declared non-Git artifacts for their leader-owned domain importers.
func ImportGit(ctx context.Context, target string, spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest, content []byte, guard Guard) ([]executioncontract.ArtifactMember, error) {
	pkg, err := executioncontract.ValidateArtifactPackage(manifest, content)
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, errors.New("remote handback: leader generation guard is required")
	}
	current, canonicalBase, err := guard(ctx)
	if err != nil {
		return nil, err
	}
	if current != spec.Fence || canonicalBase != spec.Workspace.BaseSHA || manifest.Fence != spec.Fence || manifest.Workspace.RepositoryID != spec.Workspace.RepositoryID ||
		manifest.Workspace.BaseSHA != spec.Workspace.BaseSHA || manifest.Workspace.BaseRef != spec.Workspace.BaseRef {
		return nil, ErrStale
	}
	if err := validateOutputs(spec, manifest); err != nil {
		return nil, err
	}
	if err := requireBaseAndClean(ctx, target, spec.Workspace.BaseSHA); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), ".sybra-handback-")
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
	var patch []byte
	untracked := []executioncontract.ArtifactMember{}
	nonGit := []executioncontract.ArtifactMember{}
	for i, entry := range manifest.Artifacts {
		member := pkg.Members[i]
		switch entry.Kind {
		case "git_bundle":
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
		case "git_patch":
			patch = member.Content
		case "git_untracked":
			untracked = append(untracked, member)
		default:
			nonGit = append(nonGit, member)
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
	if err := applyPatch(ctx, checkout, patch, staging); err != nil {
		return nil, err
	}
	for i, member := range untracked {
		entry := entryFor(manifest, member.Root, member.Path)
		if err := writeMember(checkout, member, entry.Mode); err != nil {
			return nil, err
		}
		untracked[i] = member
	}
	// Recheck leader state and the worktree compare-and-swap immediately before
	// canonical mutation.
	current, canonicalBase, err = guard(ctx)
	if err != nil {
		return nil, err
	}
	if current != spec.Fence || canonicalBase != spec.Workspace.BaseSHA {
		return nil, ErrStale
	}
	if err := requireBaseAndClean(ctx, target, spec.Workspace.BaseSHA); err != nil {
		return nil, err
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
	if err := applyPatch(ctx, target, patch, staging); err != nil {
		rollback()
		return nil, err
	}
	for _, member := range untracked {
		entry := entryFor(manifest, member.Root, member.Path)
		if err := writeMember(target, member, entry.Mode); err != nil {
			rollback()
			return nil, err
		}
	}
	return nonGit, nil
}

func validateOutputs(spec executioncontract.RunSpec, manifest executioncontract.ArtifactManifest) error {
	entries := make(map[string]executioncontract.ArtifactEntry, len(manifest.Artifacts))
	declared := make(map[string]bool, len(spec.ExpectedOutputs))
	for _, expected := range spec.ExpectedOutputs {
		declared[expected.Name] = true
	}
	for _, entry := range manifest.Artifacts {
		entries[entry.Name] = entry
		if declared[entry.Name] ||
			(entry.Name == "git-bundle" && entry.Kind == "git_bundle" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/commits.bundle") ||
			(entry.Name == "git-dirty-patch" && entry.Kind == "git_patch" && entry.Root == executioncontract.RootArtifact && entry.Path == "git/dirty.patch") ||
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

func applyPatch(ctx context.Context, dir string, patch []byte, scratch string) error {
	if len(patch) == 0 {
		return nil
	}
	path := filepath.Join(scratch, "dirty.patch")
	if err := os.WriteFile(path, patch, 0o600); err != nil {
		return err
	}
	return gitexec.Run(ctx, gitexec.Options{Dir: dir}, "apply", "--binary", "--", path)
}

func entryFor(manifest executioncontract.ArtifactManifest, root executioncontract.LogicalRoot, path string) executioncontract.ArtifactEntry {
	for _, entry := range manifest.Artifacts {
		if entry.Root == root && entry.Path == path {
			return entry
		}
	}
	return executioncontract.ArtifactEntry{}
}

func writeMember(root string, member executioncontract.ArtifactMember, mode uint32) error {
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
		return os.Symlink(string(member.Content), full)
	}
	perm := os.FileMode(0o600)
	if mode == 0o100755 {
		perm = 0o700
	}
	return os.WriteFile(full, member.Content, perm)
}
