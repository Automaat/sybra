package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/reject"
	"gopkg.in/yaml.v3"
)

// ErrProjectNotRegistered indicates the requested project is not present in
// the local project store. Callers can errors.Is() this to distinguish a
// permanent misconfiguration ("user never ran Create") from transient I/O
// failures, and surface it to the UI as human-required instead of looping
// on retry.
var ErrProjectNotRegistered = errors.New("project not registered locally")

// Store is the YAML-backed CRUD layer for Project metadata under dir
// (~/.sybra/projects/), plus the bare-clone root (clonesDir,
// ~/.sybra/clones/) that Create/CreateMeta clone new projects into.
//
// Every mutation is a Get (read) → modify → writeFile sequence, so locker
// guards the whole critical section per project ID — both in-process (async
// clone completion racing a UI edit in the same process) and cross-process
// (GUI server vs. sybra-cli against the same projects dir), the same
// discipline task.Store's lockTask applies to task files.
type Store struct {
	dir       string
	clonesDir string
	store     Persistence
	locker    *fsutil.KeyedLocker
	cloneBare func(context.Context, string, string) error
	// signing is atomic because SetSigningPolicy runs on the config-reload
	// goroutine while Create and the startup migration read it concurrently.
	signing atomic.Value
}

// SetSigningPolicy late-binds the deployment's commit-signing posture, which
// is resolved from config after the Store is constructed and re-applied on
// every hot reload. Unset means SigningAuto, so a caller that never sets it
// keeps the historical host-probing behavior.
func (s *Store) SetSigningPolicy(p SigningPolicy) { s.signing.Store(string(p)) }

// SigningPolicy returns the configured posture, defaulting to SigningAuto.
func (s *Store) SigningPolicy() SigningPolicy {
	v, ok := s.signing.Load().(string)
	if !ok || v == "" {
		return SigningAuto
	}
	return SigningPolicy(v)
}

// NewStore creates dir and clonesDir if they do not exist and returns a
// Store rooted there.
// NewStoreWith returns a store whose records live in p. Clones still live under
// clonesDir on disk; only the metadata moves.
func NewStoreWith(dir, clonesDir string, p Persistence) (*Store, error) {
	s, err := NewStore(dir, clonesDir)
	if err != nil {
		return nil, err
	}
	if p != nil {
		s.store = p
	}
	return s, nil
}

func NewStore(dir, clonesDir string) (*Store, error) {
	for _, d := range []string{dir, clonesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return &Store{
		dir:       dir,
		clonesDir: clonesDir,
		locker:    fsutil.NewKeyedLocker(),
		cloneBare: CloneBare,
	}, nil
}

// lock acquires the per-project write lock for id's Get-modify-writeFile
// critical section.
func (s *Store) lock(id string) (func(), error) {
	if s.store != nil {
		return s.store.Lock(id)
	}
	return s.lockFile(id)
}

func (s *Store) lockFile(id string) (func(), error) {
	unlock, err := s.locker.Lock(id, s.filePath(id))
	if err != nil {
		return nil, fmt.Errorf("lock project %s: %w", id, err)
	}
	return unlock, nil
}

// migrateMaintenanceAutoPerProjectTimeout bounds each project's git config
// call so one stalled clone (hung disk, lock contention with a concurrent
// mutation) cannot block every other project's retrofit, or the app startup
// path calling this, indefinitely.
const migrateMaintenanceAutoPerProjectTimeout = 10 * time.Second

// MigrateDisableAutoMaintenance retrofits maintenance.auto=false (see
// DisableAutoMaintenance) onto every already-registered project's clone, so
// projects created before that CloneBare step existed get the same
// protection without a re-clone. Safe to run on every startup: git config is
// idempotent. Skips a project whose clone directory is missing, or whose
// Status is not ready/unset — CreateProject's async clone path (unlike
// Store.Create) writes directly into the final ClonePath rather than a temp
// path plus atomic rename, so a project mid-clone already has a directory
// `os.Stat` succeeds against; touching it here would race CloneBare's own
// `.git/config` writes. Each project is processed under the same per-ID lock
// every other mutation holds, so it also cannot race a concurrent Delete.
// Errors are joined and returned so the caller can log without treating this
// as fatal.
func (s *Store) MigrateDisableAutoMaintenance(ctx context.Context) error {
	projects, err := s.List()
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	var errs []error
	for i := range projects {
		p := &projects[i]
		if p.ClonePath == "" {
			continue
		}
		if p.Status != "" && p.Status != ProjectStatusReady {
			continue
		}
		if err := s.disableAutoMaintenanceLocked(ctx, p.ID, p.ClonePath); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// disableAutoMaintenanceLocked re-checks id's current clone path under the
// per-project lock before touching it, so a Delete that lands between
// MigrateDisableAutoMaintenance's List() snapshot and this call is not raced.
func (s *Store) disableAutoMaintenanceLocked(ctx context.Context, id, snapshotClonePath string) error {
	unlock, err := s.lock(id)
	if err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	defer unlock()

	current, err := s.Get(id)
	if errors.Is(err, ErrProjectNotRegistered) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	if current.ClonePath != snapshotClonePath || (current.Status != "" && current.Status != ProjectStatusReady) {
		return nil
	}
	if _, statErr := os.Stat(current.ClonePath); errors.Is(statErr, os.ErrNotExist) {
		return nil
	} else if statErr != nil {
		return fmt.Errorf("%s: stat clone: %w", id, statErr)
	}

	runCtx, cancel := context.WithTimeout(ctx, migrateMaintenanceAutoPerProjectTimeout)
	defer cancel()
	if err := DisableAutoMaintenance(runCtx, current.ClonePath); err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	if err := ConfigureCommitIdentity(runCtx, current.ClonePath); err != nil {
		return fmt.Errorf("%s: configure commit identity: %w", id, err)
	}
	// Retrofit already-registered clones: nothing ever wrote the signing
	// posture, so existing projects carry whatever incidental state their
	// host happens to have.
	if err := ConfigureCommitSigning(runCtx, current.ClonePath, s.SigningPolicy()); err != nil {
		return fmt.Errorf("%s: configure commit signing: %w", id, err)
	}
	return nil
}

// List returns every registered project. A file that fails to parse is
// silently skipped rather than failing the whole call.
func (s *Store) List() ([]Project, error) {
	if s.store != nil {
		raw, err := s.store.List()
		if err != nil {
			return nil, err
		}
		for i := range raw {
			applyProjectDefaults(&raw[i])
		}
		return raw, nil
	}
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read projects dir: %w", err)
	}

	var projects []Project
	for _, p := range paths {
		proj, err := s.readFile(p)
		if err != nil {
			continue
		}
		projects = append(projects, proj)
	}
	return projects, nil
}

// Get returns the project registered under id ("owner/repo"). Returns
// ErrProjectNotRegistered (checkable via errors.Is) if no such project has
// been created.
func (s *Store) Get(id string) (Project, error) {
	if s.store != nil {
		p, err := s.store.Read(id)
		if err != nil {
			return Project{}, err
		}
		applyProjectDefaults(&p)
		return p, nil
	}
	path := s.filePath(id)
	return s.readFile(path)
}

// RawType returns a project's type exactly as recorded on disk, without the
// missing-type→pet coercion Get applies. The confidentiality guard uses this so
// a work project whose type field is absent or unknown is never mistaken for
// pet and routed to an untrusted follower.
func (s *Store) RawType(id string) (ProjectType, error) {
	if s.store != nil {
		// Read, not Get: no defaulting, so an absent type stays absent rather
		// than reading as pet.
		p, err := s.store.Read(id)
		if err != nil {
			return "", err
		}
		return p.Type, nil
	}
	data, err := os.ReadFile(s.filePath(id))
	if err != nil {
		return "", err
	}
	var raw struct {
		Type ProjectType `yaml:"type"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse project type: %w", err)
	}
	return raw.Type, nil
}

// Create parses rawURL into an owner/repo ID, registers it as cloning, clones
// it as a bare repo under clonesDir, and then persists a ready Project record.
// The clone deliberately happens outside the per-project metadata lock: a
// stalled network clone must not block edits or deletion of the project.
// Fails if a project with the same ID is already registered. See CreateMeta
// for the register-then-clone-async variant used by the GUI's non-blocking add
// flow.
func (s *Store) Create(rawURL string, ptype ProjectType) (Project, error) {
	p, err := s.CreateMeta(rawURL, ptype)
	if err != nil {
		return Project{}, err
	}

	// Create is a synchronous CLI/Wails-bound entry point with no caller
	// context to thread through. Bound the entire clone setup, not just the
	// network subprocess inside CloneBare.
	ctx, cancel := context.WithTimeout(context.Background(), networkGitTimeout)
	defer cancel()
	clonePath := p.ClonePath + ".clone-" + p.CloneGeneration
	if err := s.cloneBare(ctx, p.URL, clonePath); err != nil {
		_ = os.RemoveAll(clonePath)
		if markErr := s.markCloneError(p); markErr != nil {
			return Project{}, fmt.Errorf("clone: %w (mark error: %w)", err, markErr)
		}
		return Project{}, fmt.Errorf("clone: %w", err)
	}
	// Before publish, so the clone is never reachable without its floor. The
	// CLI never runs the startup migration, so this is the only place a
	// CLI-created project gets one.
	if err := ConfigureCommitSigning(ctx, clonePath, s.SigningPolicy()); err != nil {
		_ = os.RemoveAll(clonePath)
		_ = s.markCloneError(p)
		return Project{}, fmt.Errorf("configure commit signing: %w", err)
	}
	if err := s.publishClone(p, clonePath); err != nil {
		_ = os.RemoveAll(clonePath)
		_ = s.markCloneError(p)
		return Project{}, fmt.Errorf("mark ready: %w", err)
	}
	return s.Get(p.ID)
}

// CreateMeta writes project metadata with Status=cloning without starting the
// clone. The caller is responsible for cloning and calling MarkReady or MarkError.
func (s *Store) CreateMeta(rawURL string, ptype ProjectType) (Project, error) {
	owner, repo, err := ParseGitHubURL(rawURL)
	if err != nil {
		return Project{}, reject.New("%w", err)
	}
	if ptype == "" {
		ptype = ProjectTypePet
	}
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, reject.New("invalid project type: %s (must be pet or work)", ptype)
	}
	id := owner + "/" + repo
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	if _, err := s.Get(id); err == nil {
		return Project{}, reject.New("project %s already exists", id)
	}
	clonePath := filepath.Join(s.clonesDir, owner, repo+".git")
	cloneGeneration, err := newCloneGeneration()
	if err != nil {
		return Project{}, err
	}
	now := time.Now().UTC()
	p := Project{
		ID:              id,
		Name:            repo,
		Owner:           owner,
		Repo:            repo,
		URL:             rawURL,
		ClonePath:       clonePath,
		Type:            ptype,
		Status:          ProjectStatusCloning,
		CloneGeneration: cloneGeneration,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.writeFile(p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// publishClone atomically makes a completed temporary clone visible only if
// the same metadata record that started it still exists. Delete and a later
// re-create therefore cannot be undone by an older clone completing late.
func (s *Store) publishClone(started Project, tempPath string) error {
	unlock, err := s.lock(started.ID)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := s.Get(started.ID)
	if err != nil {
		return err
	}
	if p.CloneGeneration != started.CloneGeneration {
		return errors.New("clone superseded by a newer project record")
	}
	if err := os.Rename(tempPath, p.ClonePath); err != nil {
		return fmt.Errorf("publish clone: %w", err)
	}
	p.Status = ProjectStatusReady
	p.CloneGeneration = ""
	p.UpdatedAt = time.Now().UTC()
	return s.writeFile(p)
}

// markCloneError records a failed clone only while it still belongs to the
// same project generation. An older clone must not mark a re-created project
// as errored.
func (s *Store) markCloneError(started Project) error {
	return s.markCloneStatus(started, ProjectStatusError)
}

// MarkReadyFor records a successful asynchronous clone only if the metadata
// record that started it has not been deleted or replaced.
func (s *Store) MarkReadyFor(started Project) error {
	return s.markCloneStatus(started, ProjectStatusReady)
}

// MarkErrorFor records a failed asynchronous clone only if the metadata
// record that started it has not been deleted or replaced.
func (s *Store) MarkErrorFor(started Project) error {
	return s.markCloneError(started)
}

func (s *Store) markCloneStatus(started Project, status ProjectStatus) error {
	unlock, err := s.lock(started.ID)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := s.Get(started.ID)
	if err != nil {
		return err
	}
	if p.CloneGeneration != started.CloneGeneration {
		return errors.New("clone superseded by a newer project record")
	}
	p.Status = status
	p.CloneGeneration = ""
	p.UpdatedAt = time.Now().UTC()
	return s.writeFile(p)
}

func newCloneGeneration() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate clone generation: %w", err)
	}
	return hex.EncodeToString(data), nil
}

// MarkReady transitions a project from cloning to ready after a successful clone.
func (s *Store) MarkReady(id string) error {
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return err
	}
	p.Status = ProjectStatusReady
	p.CloneGeneration = ""
	p.UpdatedAt = time.Now().UTC()
	return s.writeFile(p)
}

// MarkError transitions a project to the error state when cloning fails.
func (s *Store) MarkError(id string) error {
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return err
	}
	p.Status = ProjectStatusError
	p.CloneGeneration = ""
	p.UpdatedAt = time.Now().UTC()
	return s.writeFile(p)
}

// Update sets the project type ("pet" or "work") for an existing project.
// For other fields see SetSandboxConfig, SetSetupCommands, and
// SetWorktreeBaseRef.
func (s *Store) Update(id string, ptype ProjectType) (Project, error) {
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, reject.New("invalid project type: %s (must be pet or work)", ptype)
	}
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return p, err
	}
	p.Type = ptype
	p.UpdatedAt = time.Now().UTC()
	return p, s.writeFile(p)
}

// SetSandboxConfig replaces the sandbox configuration for a project.
func (s *Store) SetSandboxConfig(id string, cfg *SandboxConfig) (Project, error) {
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return p, err
	}
	p.Sandbox = cfg
	p.UpdatedAt = time.Now().UTC()
	return p, s.writeFile(p)
}

// SetSetupCommands replaces the setup commands for a project.
func (s *Store) SetSetupCommands(id string, cmds []string) (Project, error) {
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return p, err
	}
	p.SetupCommands = cmds
	p.UpdatedAt = time.Now().UTC()
	return p, s.writeFile(p)
}

// SetWorktreeBaseRef sets the worktree branching base for a project.
// ref must be WorktreeBaseRefFresh or WorktreeBaseRefHead.
func (s *Store) SetWorktreeBaseRef(id, ref string) (Project, error) {
	if ref != WorktreeBaseRefFresh && ref != WorktreeBaseRefHead {
		return Project{}, reject.New("invalid worktree_base_ref %q (must be %q or %q)", ref, WorktreeBaseRefFresh, WorktreeBaseRefHead)
	}
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return p, err
	}
	p.WorktreeBaseRef = ref
	p.UpdatedAt = time.Now().UTC()
	return p, s.writeFile(p)
}

// Delete removes project id's bare clone directory (if any) and its YAML
// metadata file. It does not touch any per-task worktrees already checked
// out from that clone.
func (s *Store) Delete(id string) error {
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := s.Get(id)
	if err != nil {
		return err
	}

	if p.ClonePath != "" {
		_ = os.RemoveAll(p.ClonePath)
	}

	if s.store != nil {
		// The record lives in the database, and there is no file to remove.
		// Removing one and leaving the row would relist a project whose clone
		// this call just deleted — the exact disagreement this change exists
		// to prevent.
		return s.store.Delete(id)
	}
	path := s.filePath(id)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete project file: %w", err)
	}
	return nil
}

func (s *Store) filePath(id string) string {
	// owner/repo → owner--repo.yaml
	safe := strings.ReplaceAll(id, "/", "--")
	return filepath.Join(s.dir, safe+".yaml")
}

// applyProjectDefaults fills the fields an older record may omit. Both backends
// apply it on the read paths that want it, so a project stored either way
// behaves identically.
func applyProjectDefaults(p *Project) {
	if p.Type == "" {
		p.Type = ProjectTypePet
	}
	if p.Status == "" {
		p.Status = ProjectStatusReady
	}
	if p.WorktreeBaseRef == "" {
		p.WorktreeBaseRef = WorktreeBaseRefFresh
	}
}

func (s *Store) readFile(path string) (Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Project{}, fmt.Errorf("read project %s: %w", path, ErrProjectNotRegistered)
		}
		return Project{}, fmt.Errorf("read project: %w", err)
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Project{}, fmt.Errorf("parse project: %w", err)
	}
	if p.Type == "" {
		p.Type = ProjectTypePet
	}
	if p.Status == "" {
		p.Status = ProjectStatusReady
	}
	if p.WorktreeBaseRef == "" {
		p.WorktreeBaseRef = WorktreeBaseRefFresh
	}
	return p, nil
}

func (s *Store) writeFile(p Project) error {
	if s.store != nil {
		return s.store.Write(p)
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal project: %w", err)
	}
	return fsutil.AtomicWrite(s.filePath(p.ID), data)
}
