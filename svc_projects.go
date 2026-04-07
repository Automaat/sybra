package main

import (
	"log/slog"
	"os/exec"

	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/worktree"
)

// ProjectService exposes project and worktree operations as Wails-bound methods.
type ProjectService struct {
	projects  *project.Store
	worktrees *worktree.Manager
	logger    *slog.Logger
}

// ListProjects returns all registered projects.
func (s *ProjectService) ListProjects() ([]project.Project, error) {
	return s.projects.List()
}

// GetProject returns a single project by ID.
func (s *ProjectService) GetProject(id string) (project.Project, error) {
	return s.projects.Get(id)
}

// CreateProject clones a GitHub repo as a bare mirror and registers it.
func (s *ProjectService) CreateProject(url, ptype string) (project.Project, error) {
	s.logger.Info("project.create", "url", url, "type", ptype)
	p, err := s.projects.Create(url, project.ProjectType(ptype))
	if err != nil {
		s.logger.Error("project.create.failed", "url", url, "err", err)
		return p, err
	}
	s.logger.Info("project.created", "id", p.ID, "url", url)
	return p, nil
}

// UpdateProject changes the type (pet/work) of a registered project.
func (s *ProjectService) UpdateProject(id, ptype string) (project.Project, error) {
	s.logger.Info("project.update", "id", id, "type", ptype)
	p, err := s.projects.Update(id, project.ProjectType(ptype))
	if err != nil {
		s.logger.Error("project.update.failed", "id", id, "err", err)
		return p, err
	}
	return p, nil
}

// DeleteProject removes a project and its bare clone from disk.
func (s *ProjectService) DeleteProject(id string) error {
	s.logger.Info("project.delete", "id", id)
	if err := s.projects.Delete(id); err != nil {
		s.logger.Error("project.delete.failed", "id", id, "err", err)
		return err
	}
	return nil
}

// ListWorktrees returns all git worktrees for the given project's bare clone.
func (s *ProjectService) ListWorktrees(projectID string) ([]project.Worktree, error) {
	return s.worktrees.List(projectID)
}

// OpenInTerminal opens a worktree path in a new Ghostty terminal tab.
func (s *ProjectService) OpenInTerminal(path string) error {
	if err := s.worktrees.ValidatePath(path); err != nil {
		return err
	}
	return openDirInGhostty(path)
}

// OpenInEditor opens a worktree path in Zed.
func (s *ProjectService) OpenInEditor(path string) error {
	if err := s.worktrees.ValidatePath(path); err != nil {
		return err
	}
	return exec.Command("zed", path).Start()
}
