package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
	"gopkg.in/yaml.v3"
)

type Store struct {
	dir       string
	clonesDir string
}

func NewStore(dir, clonesDir string) (*Store, error) {
	for _, d := range []string{dir, clonesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return &Store{dir: dir, clonesDir: clonesDir}, nil
}

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

func (s *Store) Get(id string) (Project, error) {
	path := s.filePath(id)
	return s.readFile(path)
}

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
	if _, err := s.Get(id); err == nil {
		return Project{}, fmt.Errorf("project %s already exists", id)
	}

	clonePath := filepath.Join(s.clonesDir, owner, repo+".git")
	if err := CloneBare(rawURL, clonePath); err != nil {
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
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.writeFile(p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (s *Store) Update(id string, ptype ProjectType) (Project, error) {
	if ptype != ProjectTypePet && ptype != ProjectTypeWork {
		return Project{}, fmt.Errorf("invalid project type: %s (must be pet or work)", ptype)
	}
	p, err := s.Get(id)
	if err != nil {
		return p, err
	}
	p.Type = ptype
	p.UpdatedAt = time.Now().UTC()
	return p, s.writeFile(p)
}

func (s *Store) Delete(id string) error {
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
		return Project{}, fmt.Errorf("read project: %w", err)
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Project{}, fmt.Errorf("parse project: %w", err)
	}
	if p.Type == "" {
		p.Type = ProjectTypePet
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
