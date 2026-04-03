package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read projects dir: %w", err)
	}

	var projects []Project
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := s.readFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *Store) Get(id string) (Project, error) {
	path := s.filePath(id)
	return s.readFile(path)
}

func (s *Store) Create(rawURL string) (Project, error) {
	owner, repo, err := ParseGitHubURL(rawURL)
	if err != nil {
		return Project{}, err
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
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.writeFile(p); err != nil {
		return Project{}, err
	}
	return p, nil
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
	return p, nil
}

func (s *Store) writeFile(p Project) error {
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal project: %w", err)
	}
	path := s.filePath(p.ID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write project file: %w", err)
	}
	return nil
}
