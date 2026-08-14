// Package agentworkspace prepares immutable daemon checkouts and produces
// bounded, content-addressed handback packages without board-store access.
package agentworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/notes"
)

const (
	stagedPatchPath   = "git/staged.patch"
	unstagedPatchPath = "git/unstaged.patch"
)

var (
	ErrImmutableBaseUnavailable = errors.New("agent workspace: immutable base is unavailable")
	ErrInvalidBaseBundle        = errors.New("agent workspace: invalid base bundle")
	ErrRepositoryAnchorMoved    = errors.New("agent workspace: selected repository anchor is unavailable")
)

type Layout struct {
	RunRoot       string
	Worktree      string
	Sidecar       string
	Artifact      string
	WorkingMemory string
}

func (l Layout) Root(root executioncontract.LogicalRoot) string {
	switch root {
	case executioncontract.RootWorktree:
		return l.Worktree
	case executioncontract.RootSidecar:
		return l.Sidecar
	case executioncontract.RootArtifact:
		return l.Artifact
	case executioncontract.RootWorkingMemory:
		return l.WorkingMemory
	default:
		return ""
	}
}

// BaseBundleRef is the private ref used on both sides of the execution
// boundary. Hashing the protocol identity keeps every valid RunID safe for Git
// ref syntax without weakening the contract's broader identity language.
func BaseBundleRef(runID string) string {
	digest := sha256.Sum256([]byte(runID))
	return "refs/sybra/base-input/" + hex.EncodeToString(digest[:])
}

// Prepare clones the daemon-local source and checks out exactly BaseSHA. The
// mutable BaseRef tip is verified only as ancestry; it is never checked out.
func Prepare(ctx context.Context, root, source string, spec executioncontract.RunSpec) (Layout, error) {
	return PrepareWithBaseBundle(ctx, root, source, spec, nil)
}

// PrepareWithBaseBundle imports a content-addressed thin Git bundle before
// resolving BaseSHA. Bundle verification happens inside the disposable clone,
// so missing prerequisites, forged refs, and corrupt bytes fail closed without
// mutating daemon source state.
func PrepareWithBaseBundle(ctx context.Context, root, source string, spec executioncontract.RunSpec, baseBundle []byte) (Layout, error) {
	if err := spec.Validate(); err != nil {
		return Layout{}, err
	}
	if strings.TrimSpace(source) == "" {
		return Layout{}, fmt.Errorf("agent workspace: repository %q is not configured", spec.Workspace.RepositoryID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Layout{}, err
	}
	runRoot := filepath.Join(root, spec.RunID)
	if _, err := os.Stat(runRoot); err == nil {
		return Layout{}, errors.New("agent workspace: run workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Layout{}, err
	}
	tmp, err := os.MkdirTemp(root, ".prepare-"+spec.RunID+"-")
	if err != nil {
		return Layout{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	worktree := filepath.Join(tmp, "worktree")
	if err := cloneAtRepositoryAnchor(ctx, source, worktree, spec.Workspace.RepositoryAnchor); err != nil {
		return Layout{}, err
	}
	baseRef := spec.Workspace.BaseRef
	if ref := spec.Workspace.BaseBundle; ref != nil {
		if int64(len(baseBundle)) != ref.SizeBytes || int64(len(baseBundle)) > executioncontract.MaxWorkspaceBaseBundleSize ||
			!strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(baseBundle)), ref.DigestSHA256) {
			return Layout{}, fmt.Errorf("%w: content differs from contract", ErrInvalidBaseBundle)
		}
		bundlePath := filepath.Join(tmp, ".base.bundle")
		if err := os.WriteFile(bundlePath, baseBundle, 0o600); err != nil {
			return Layout{}, err
		}
		if err := gitexec.Run(ctx, gitexec.Options{Dir: worktree}, "bundle", "verify", bundlePath); err != nil {
			return Layout{}, fmt.Errorf("%w: verify: %w", ErrInvalidBaseBundle, err)
		}
		importedRef := BaseBundleRef(spec.RunID)
		if err := gitexec.Run(ctx, gitexec.Options{Dir: worktree}, "fetch", bundlePath, importedRef+":"+importedRef); err != nil {
			return Layout{}, fmt.Errorf("%w: import: %w", ErrInvalidBaseBundle, err)
		}
		_ = os.Remove(bundlePath)
		resolved, err := gitexec.Output(ctx, gitexec.Options{Dir: worktree}, "rev-parse", "--verify", importedRef+"^{commit}")
		if err != nil || resolved != spec.Workspace.BaseSHA {
			return Layout{}, fmt.Errorf("%w: immutable base is not advertised", ErrInvalidBaseBundle)
		}
		baseRef = importedRef
	} else if len(baseBundle) != 0 {
		return Layout{}, fmt.Errorf("%w: unexpected content", ErrInvalidBaseBundle)
	}
	if _, err := gitexec.Output(ctx, gitexec.Options{Dir: worktree}, "rev-parse", "--verify", spec.Workspace.BaseSHA+"^{commit}"); err != nil {
		return Layout{}, fmt.Errorf("%w: %w", ErrImmutableBaseUnavailable, err)
	}
	if branch, ok := strings.CutPrefix(baseRef, "refs/heads/"); ok {
		remoteRef := "refs/remotes/origin/" + branch
		if _, err := gitexec.Output(ctx, gitexec.Options{Dir: worktree}, "rev-parse", "--verify", remoteRef+"^{commit}"); err == nil {
			baseRef = remoteRef
		}
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: worktree}, "merge-base", "--is-ancestor", spec.Workspace.BaseSHA, baseRef); err != nil {
		return Layout{}, fmt.Errorf("agent workspace: base SHA is not an ancestor of base ref: %w", err)
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: worktree}, "checkout", "--detach", spec.Workspace.BaseSHA); err != nil {
		return Layout{}, fmt.Errorf("agent workspace: checkout immutable base: %w", err)
	}
	// Remote workspaces must be able to create handback commits without
	// depending on the daemon account's operator identity or signing setup.
	// These repository-local values stay inside the disposable checkout.
	for key, value := range map[string]string{
		"user.name":      "Sybra Agent",
		"user.email":     "sybra-agent@example.invalid",
		"commit.gpgsign": "false",
		"tag.gpgsign":    "false",
	} {
		if err := gitexec.Run(ctx, gitexec.Options{Dir: worktree}, "config", "--local", key, value); err != nil {
			return Layout{}, fmt.Errorf("agent workspace: configure git identity: %w", err)
		}
	}
	layout := Layout{RunRoot: tmp, Worktree: worktree, Sidecar: filepath.Join(tmp, "sidecar"), Artifact: filepath.Join(tmp, "artifact"), WorkingMemory: worktree}
	for _, declared := range spec.Workspace.Roots {
		path := layout.Root(declared)
		if path != worktree {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return Layout{}, err
			}
		}
	}
	for _, excluded := range []string{notes.FileName, ".sybra-evidence"} {
		if err := exclude(ctx, worktree, excluded); err != nil {
			return Layout{}, fmt.Errorf("agent workspace: exclude private path: %w", err)
		}
	}
	if spec.Options.SeedWorkingMemory {
		if err := os.WriteFile(filepath.Join(worktree, notes.FileName), []byte(notes.SeedTemplate), 0o600); err != nil {
			return Layout{}, err
		}
	}
	final := Layout{RunRoot: runRoot, Worktree: filepath.Join(runRoot, "worktree"), Sidecar: filepath.Join(runRoot, "sidecar"), Artifact: filepath.Join(runRoot, "artifact"), WorkingMemory: filepath.Join(runRoot, "worktree")}
	if err := os.Rename(tmp, runRoot); err != nil {
		return Layout{}, err
	}
	return final, nil
}

func cloneAtRepositoryAnchor(ctx context.Context, source, worktree, anchor string) error {
	if err := gitexec.Run(ctx, gitexec.Options{}, "clone", "--no-checkout", "--no-local", "--", source, worktree); err != nil {
		return fmt.Errorf("agent workspace: clone repository: %w", err)
	}
	if anchor != "" {
		if _, err := gitexec.Output(ctx, gitexec.Options{Dir: worktree}, "rev-parse", "--verify", anchor+"^{commit}"); err != nil {
			return fmt.Errorf("%w: %w", ErrRepositoryAnchorMoved, err)
		}
	}
	return nil
}

func exclude(ctx context.Context, worktree, path string) error {
	excludePath, err := gitexec.Output(ctx, gitexec.Options{Dir: worktree}, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktree, excludePath)
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintln(f, path)
	return err
}

func Environment(layout Layout) []string {
	return []string{
		"SYBRA_WORKTREE_ROOT=" + layout.Worktree,
		"SYBRA_SIDECAR_ROOT=" + layout.Sidecar,
		"SYBRA_ARTIFACT_ROOT=" + layout.Artifact,
		"SYBRA_WORKING_MEMORY_ROOT=" + layout.WorkingMemory,
	}
}

//nolint:funlen // Collection keeps package-member and manifest-entry construction in one auditable order.
func Collect(ctx context.Context, layout Layout, spec executioncontract.RunSpec, build string) (executioncontract.ArtifactManifest, []byte, error) {
	index, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: layout.Worktree}, "ls-files", "--debug", "--", ".")
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	if strings.Contains(string(index), "flags: 20004000") {
		return executioncontract.ArtifactManifest{}, nil, errors.New("agent workspace: intent-to-add index entries cannot be handed back exactly")
	}
	finalSHA, err := gitexec.Output(ctx, gitexec.Options{Dir: layout.Worktree}, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: layout.Worktree}, "merge-base", "--is-ancestor", spec.Workspace.BaseSHA, finalSHA); err != nil {
		return executioncontract.ArtifactManifest{}, nil, errors.New("agent workspace: final commit does not descend from immutable base")
	}
	type collected struct {
		entry executioncontract.ArtifactEntry
		data  []byte
	}
	items := []collected{}
	add := func(name, kind string, root executioncontract.LogicalRoot, path, media string, sensitivity executioncontract.Sensitivity, mode uint32, data []byte) error {
		if int64(len(data)) > executioncontract.MaxArtifactEntrySize {
			return fmt.Errorf("agent workspace: output %q exceeds size limit", name)
		}
		items = append(items, collected{entry: executioncontract.ArtifactEntry{
			Name: name, Kind: kind, Root: root, Path: path, DigestSHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
			SizeBytes: int64(len(data)), MediaType: media, Sensitivity: sensitivity, Mode: mode,
		}, data: append([]byte(nil), data...)})
		return nil
	}
	if finalSHA != spec.Workspace.BaseSHA {
		ref := "refs/sybra/handback/" + spec.RunID
		if err := gitexec.Run(ctx, gitexec.Options{Dir: layout.Worktree}, "update-ref", ref, finalSHA); err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
		defer func() {
			_ = gitexec.Run(context.WithoutCancel(ctx), gitexec.Options{Dir: layout.Worktree}, "update-ref", "-d", ref)
		}()
		bundlePath := filepath.Join(layout.RunRoot, "handback.bundle")
		if err := gitexec.Run(ctx, gitexec.Options{Dir: layout.Worktree}, "bundle", "create", bundlePath, ref, "^"+spec.Workspace.BaseSHA); err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
		bundle, err := os.ReadFile(bundlePath)
		if err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
		if err := add("git-bundle", "git_bundle", executioncontract.RootArtifact, "git/commits.bundle", "application/x-git-bundle", executioncontract.SensitivityInternal, 0, bundle); err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
	}
	stagedPatch, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: layout.Worktree}, "diff", "--cached", "--binary", "--full-index", "HEAD", "--", ".")
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	if err := add("git-staged-patch", "git_staged_patch", executioncontract.RootArtifact, stagedPatchPath, "application/x-git-patch", executioncontract.SensitivityInternal, 0, stagedPatch); err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	unstagedPatch, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: layout.Worktree}, "diff", "--binary", "--full-index", "--", ".")
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	if err := add("git-unstaged-patch", "git_unstaged_patch", executioncontract.RootArtifact, unstagedPatchPath, "application/x-git-patch", executioncontract.SensitivityInternal, 0, unstagedPatch); err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	untrackedRaw, err := gitexec.RawOutput(ctx, gitexec.Options{Dir: layout.Worktree}, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	untracked := splitNUL(untrackedRaw)
	sort.Strings(untracked)
	privatePaths := make(map[string]bool)
	for _, output := range spec.ExpectedOutputs {
		if output.Root == executioncontract.RootWorkingMemory {
			privatePaths[output.Path] = true
		}
	}
	for _, rel := range untracked {
		if rel == notes.FileName || strings.HasPrefix(rel, ".sybra-evidence/") || privatePaths[rel] {
			continue
		}
		data, mode, err := readRegularOrSymlink(layout.Worktree, rel)
		if err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
		if err := add("untracked:"+rel, "git_untracked", executioncontract.RootWorktree, rel, "application/octet-stream", executioncontract.SensitivityInternal, mode, data); err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
	}
	outputs := append([]executioncontract.ExpectedOutput(nil), spec.ExpectedOutputs...)
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].Name < outputs[j].Name })
	for _, output := range outputs {
		if output.Root == executioncontract.RootWorkingMemory && !spec.Options.SeedWorkingMemory {
			if output.Required {
				return executioncontract.ArtifactManifest{}, nil, errors.New("agent workspace: verifier run cannot return working memory")
			}
			continue
		}
		full := filepath.Join(layout.Root(output.Root), filepath.FromSlash(output.Path))
		data, mode, err := readRegularOrSymlink(layout.Root(output.Root), output.Path)
		if errors.Is(err, os.ErrNotExist) && !output.Required {
			continue
		}
		if err != nil {
			return executioncontract.ArtifactManifest{}, nil, fmt.Errorf("agent workspace: collect declared output %q at %s: %w", output.Name, full, err)
		}
		limit := output.MaxBytes
		if limit == 0 {
			limit = executioncontract.MaxArtifactEntrySize
		}
		if int64(len(data)) > limit {
			return executioncontract.ArtifactManifest{}, nil, fmt.Errorf("agent workspace: declared output %q exceeds its size limit", output.Name)
		}
		mediaType := mime.TypeByExtension(filepath.Ext(output.Path))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if base, _, ok := strings.Cut(mediaType, ";"); ok {
			mediaType = base
		}
		if len(output.MediaTypes) > 0 && !slices.Contains(output.MediaTypes, mediaType) {
			return executioncontract.ArtifactManifest{}, nil, fmt.Errorf("agent workspace: declared output %q has forbidden media type %q", output.Name, mediaType)
		}
		if err := add(output.Name, output.Kind, output.Root, output.Path, mediaType, output.Sensitivity, mode, data); err != nil {
			return executioncontract.ArtifactManifest{}, nil, err
		}
	}
	slices.SortFunc(items, func(a, b collected) int {
		if a.entry.Root != b.entry.Root {
			return strings.Compare(string(a.entry.Root), string(b.entry.Root))
		}
		return strings.Compare(a.entry.Path, b.entry.Path)
	})
	entries := make([]executioncontract.ArtifactEntry, len(items))
	pkg := executioncontract.ArtifactPackage{Members: make([]executioncontract.ArtifactMember, len(items))}
	for i := range items {
		entries[i] = items[i].entry
		pkg.Members[i] = executioncontract.ArtifactMember{Root: items[i].entry.Root, Path: items[i].entry.Path, Content: items[i].data}
	}
	content, err := json.Marshal(pkg)
	if err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	manifestDigest := sha256.New()
	_, _ = manifestDigest.Write([]byte(spec.RunID))
	_, _ = manifestDigest.Write([]byte{0})
	_, _ = manifestDigest.Write(content)
	manifest := executioncontract.ArtifactManifest{
		Version: executioncontract.CurrentVersion(), BuildVersion: build, RunID: spec.RunID,
		ManifestID: "manifest-" + hex.EncodeToString(manifestDigest.Sum(nil)), IdempotencyKey: spec.RunID + ":artifacts:v1",
		State: executioncontract.ArtifactsReady, GeneratedAt: time.Now().UTC(), Fence: spec.Fence,
		Workspace: executioncontract.WorkspaceHandback{RepositoryID: spec.Workspace.RepositoryID, BaseSHA: spec.Workspace.BaseSHA, BaseRef: spec.Workspace.BaseRef, FinalSHA: finalSHA},
		Artifacts: entries,
	}
	if _, err := executioncontract.ValidateArtifactPackage(manifest, content); err != nil {
		return executioncontract.ArtifactManifest{}, nil, err
	}
	return manifest, content, nil
}

func splitNUL(data []byte) []string {
	parts := make([]string, 0)
	for part := range strings.SplitSeq(string(data), "\x00") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func readRegularOrSymlink(root, rel string) (data []byte, mode uint32, err error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, 0, err
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return nil, 0, err
	}
	relParent, err := filepath.Rel(rootReal, parentReal)
	if err != nil || relParent == ".." || strings.HasPrefix(relParent, ".."+string(filepath.Separator)) {
		return nil, 0, errors.New("agent workspace: output parent escapes logical root")
	}
	info, err := os.Lstat(full)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		return []byte(target), 0o120000, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("agent workspace: output must be a regular file or symlink")
	}
	data, err = os.ReadFile(full)
	mode = uint32(0o100644)
	if info.Mode()&0o111 != 0 {
		mode = 0o100755
	}
	return data, mode, err
}
