package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
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
	locker    *fsutil.KeyedLocker
}

// NewStore creates dir and clonesDir if they do not exist and returns a
// Store rooted there.
func NewStore(dir, clonesDir string) (*Store, error) {
	for _, d := range []string{dir, clonesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return &Store{dir: dir, clonesDir: clonesDir, locker: fsutil.NewKeyedLocker()}, nil
}

// lock acquires the per-project write lock for id's Get-modify-writeFile
// critical section.
func (s *Store) lock(id string) (func(), error) {
	unlock, err := s.locker.Lock(id, s.filePath(id))
	if err != nil {
		return nil, fmt.Errorf("lock project %s: %w", id, err)
	}
	return unlock, nil
}

// List returns every registered project. A file that fails to parse is
// silently skipped rather than failing the whole call.
func (s *Store) List() ([]Project, error) {
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
	path := s.filePath(id)
	return s.readFile(path)
}

// RawType returns a project's type exactly as recorded on disk, without the
// missing-type→pet coercion Get applies. The confidentiality guard uses this so
// a work project whose type field is absent or unknown is never mistaken for
// pet and routed to an untrusted follower.
func (s *Store) RawType(id string) (ProjectType, error) {
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

// Create parses rawURL into an owner/repo ID, clones it as a bare repo
// under clonesDir, and persists a ready Project record. Fails if a project
// with the same ID is already registered. See CreateMeta for the
// register-then-clone-async variant used by the GUI's non-blocking add flow.
func (s *Store) Create(rawURL string, ptype ProjectType) (Project, error) {
	owner, repo, err := ParseGitHubURL(rawURL)
	if err != nil {
		return Project{}, err
	}

	if ptype == "" {
		ptype = ProjectTypePet
	}
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, fmt.Errorf("invalid project type: %s (must be pet or work)", ptype)
	}

	id := owner + "/" + repo
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	if _, err := s.Get(id); err == nil {
		return Project{}, fmt.Errorf("project %s already exists", id)
	}

	clonePath := filepath.Join(s.clonesDir, owner, repo+".git")
	// context.Background(): Create is a synchronous CLI/Wails-bound entry
	// point (cmd/sybra-cli and ProjectService.CreateProject) with no ctx to
	// thread through.
	if err := CloneBare(context.Background(), rawURL, clonePath); err != nil {
		return Project{}, fmt.Errorf("clone: %w", err)
	}

	now := time.Now().UTC()
	p := Project{
		ID:        id,
		Name:      repo,
		Owner:     owner,
		Repo:      repo,
		URL:       rawURL,
		ClonePath: clonePath,
		Type:      ptype,
		Status:    ProjectStatusReady,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.writeFile(p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// CreateMeta writes project metadata with Status=cloning without starting the
// clone. The caller is responsible for cloning and calling MarkReady or MarkError.
func (s *Store) CreateMeta(rawURL string, ptype ProjectType) (Project, error) {
	owner, repo, err := ParseGitHubURL(rawURL)
	if err != nil {
		return Project{}, err
	}
	if ptype == "" {
		ptype = ProjectTypePet
	}
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, fmt.Errorf("invalid project type: %s (must be pet or work)", ptype)
	}
	id := owner + "/" + repo
	unlock, err := s.lock(id)
	if err != nil {
		return Project{}, err
	}
	defer unlock()

	if _, err := s.Get(id); err == nil {
		return Project{}, fmt.Errorf("project %s already exists", id)
	}
	clonePath := filepath.Join(s.clonesDir, owner, repo+".git")
	now := time.Now().UTC()
	p := Project{
		ID:        id,
		Name:      repo,
		Owner:     owner,
		Repo:      repo,
		URL:       rawURL,
		ClonePath: clonePath,
		Type:      ptype,
		Status:    ProjectStatusCloning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.writeFile(p); err != nil {
		return Project{}, err
	}
	return p, nil
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
	p.UpdatedAt = time.Now().UTC()
	return s.writeFile(p)
}

// Update sets the project type ("pet" or "work") for an existing project.
// For other fields see SetSandboxConfig, SetSetupCommands, and
// SetWorktreeBaseRef.
func (s *Store) Update(id string, ptype ProjectType) (Project, error) {
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, fmt.Errorf("invalid project type: %s (must be pet or work)", ptype)
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
		return Project{}, fmt.Errorf("invalid worktree_base_ref %q (must be %q or %q)", ref, WorktreeBaseRefFresh, WorktreeBaseRefHead)
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
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal project: %w", err)
	}
	return fsutil.AtomicWrite(s.filePath(p.ID), data)
}
