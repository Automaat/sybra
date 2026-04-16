package sybra

import (
	"log/slog"
	"os/exec"
	"sync"

	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/worktree"
)

// ProjectService exposes project and worktree operations as Wails-bound methods.
type ProjectService struct {
	projects  *project.Store
	worktrees *worktree.Manager
	logger    *slog.Logger
	notifier  *notification.Emitter
	bgops     *bgop.Tracker
	wg        *sync.WaitGroup
}

// ListProjects returns all registered projects.
func (s *ProjectService) ListProjects() ([]project.Project, error) {
	return s.projects.List()
}

// GetProject returns a single project by ID.
func (s *ProjectService) GetProject(id string) (project.Project, error) {
	return s.projects.Get(id)
}

// CreateProject registers a GitHub repo and starts a bare clone in the
// background. It returns immediately with the project in cloning status.
func (s *ProjectService) CreateProject(url, ptype string) (project.Project, error) {
	s.logger.Info("project.create", "url", url, "type", ptype)
	p, err := s.projects.CreateMeta(url, project.ProjectType(ptype))
	if err != nil {
		s.logger.Error("project.create.failed", "url", url, "err", err)
		return p, err
	}

	opID := ""
	if s.bgops != nil {
		opID = s.bgops.Start(bgop.TypeClone, "Cloning "+p.Owner+"/"+p.Repo, p.ID, "")
	}
	s.logger.Info("project.clone.started", "id", p.ID, "op", opID)

	s.wg.Go(func() {
		if err := project.CloneBare(p.URL, p.ClonePath); err != nil {
			s.logger.Error("project.clone.failed", "id", p.ID, "err", err)
			if markErr := s.projects.MarkError(p.ID); markErr != nil {
				s.logger.Error("project.mark-error", "id", p.ID, "err", markErr)
			}
			if s.bgops != nil && opID != "" {
				s.bgops.Fail(opID, err)
			}
			if s.notifier != nil {
				s.notifier.Send(notification.LevelError, "Clone failed",
					p.Owner+"/"+p.Repo+": "+err.Error(), "", "")
			}
			return
		}
		if markErr := s.projects.MarkReady(p.ID); markErr != nil {
			s.logger.Error("project.mark-ready", "id", p.ID, "err", markErr)
		}
		if s.bgops != nil && opID != "" {
			s.bgops.Complete(opID)
		}
		if s.notifier != nil {
			s.notifier.Send(notification.LevelSuccess, "Project cloned",
				p.Owner+"/"+p.Repo+" is ready", "", "")
		}
		s.logger.Info("project.cloned", "id", p.ID)
	})

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

// SetProjectSandboxConfig replaces the sandbox configuration for a project.
func (s *ProjectService) SetProjectSandboxConfig(id string, cfg *project.SandboxConfig) (project.Project, error) {
	s.logger.Info("project.set-sandbox-config", "id", id)
	p, err := s.projects.SetSandboxConfig(id, cfg)
	if err != nil {
		s.logger.Error("project.set-sandbox-config.failed", "id", id, "err", err)
	}
	return p, err
}

// SetProjectSetupCommands replaces the setup commands for a project.
func (s *ProjectService) SetProjectSetupCommands(id string, cmds []string) (project.Project, error) {
	s.logger.Info("project.set-setup-commands", "id", id, "count", len(cmds))
	p, err := s.projects.SetSetupCommands(id, cmds)
	if err != nil {
		s.logger.Error("project.set-setup-commands.failed", "id", id, "err", err)
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
