package sybra

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/reject"
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
	p, err := s.projects.Get(id)
	return p, boardRejectionFor("project", id, err)
}

// GetProjectRawType returns a project's type exactly as recorded, without
// GetProject's missing-type→pet coercion.
//
// A client cannot derive this from GetProject: the confidentiality guard needs
// "unset" to stay distinguishable from "pet" so a work project with an absent
// type field is never routed to an untrusted follower, and GetProject has
// already collapsed the two by the time the record reaches the wire.
func (s *ProjectService) GetProjectRawType(id string) (string, error) {
	raw, err := s.projects.RawType(id)
	if err != nil {
		return "", boardRejectionFor("project", id, err)
	}
	return string(raw), nil
}

// CreateProjectAndClone registers a repo and finishes its clone before
// returning, reporting a clone failure to the caller.
//
// CreateProject's background clone suits the GUI, which watches the record
// flip out of `cloning`. A CLI caller has nothing to watch: it exits, so an
// async create would print success on a repo that never cloned and hand back
// a record in `cloning` where the filesystem-backed command returned `ready`.
func (s *ProjectService) CreateProjectAndClone(url, ptype string) (project.Project, error) {
	s.logger.Info("project.create.sync", "url", url, "type", ptype)
	p, err := s.projects.Create(url, project.ProjectType(ptype))
	if err != nil {
		s.logger.Error("project.create.sync.failed", "url", url, "err", err)
		if reject.Is(err) {
			return project.Project{}, validationError(err.Error())
		}
		// A clone failure wraps the git invocation, which carries the
		// server's clone path. It stays in the log rather than going on
		// the wire; the caller still gets a loud, non-generic failure.
		return project.Project{}, unavailableError("clone failed; see the server log for the git error")
	}
	return p, nil
}

// CreateProject registers a GitHub repo and starts a bare clone in the
// background. It returns immediately with the project in cloning status.
func (s *ProjectService) CreateProject(url, ptype string) (project.Project, error) {
	s.logger.Info("project.create", "url", url, "type", ptype)
	p, err := s.projects.CreateMeta(url, project.ProjectType(ptype))
	if err != nil {
		s.logger.Error("project.create.failed", "url", url, "err", err)
		return p, boardRejection(err)
	}

	opID := ""
	if s.bgops != nil {
		opID = s.bgops.Start(bgop.TypeClone, "Cloning "+p.Owner+"/"+p.Repo, p.ID, "")
	}
	s.logger.Info("project.clone.started", "id", p.ID, "op", opID)

	s.wg.Go(func() {
		// context.Background(): CreateProject is a Wails-bound method; this
		// runs in a detached background goroutine with no ctx to thread.
		if err := project.CloneBare(context.Background(), p.URL, p.ClonePath); err != nil {
			s.logger.Error("project.clone.failed", "id", p.ID, "err", err)
			if markErr := s.projects.MarkErrorFor(p); markErr != nil {
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
		// Non-gating: the startup migration retries this, and an otherwise
		// healthy clone should not be marked failed over it.
		if err := project.ConfigureCommitSigning(context.Background(), p.ClonePath, s.projects.SigningPolicy()); err != nil {
			s.logger.Warn("project.clone.commit-signing", "id", p.ID, "err", err)
		}
		if markErr := s.projects.MarkReadyFor(p); markErr != nil {
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
		return p, boardRejectionFor("project", id, err)
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
		return p, boardRejectionFor("project", id, err)
	}
	return p, nil
}

// SetProjectWorktreeBaseRef sets the worktree branching base for a project.
// ref must be "fresh" (branch off origin/<default>) or "head" (branch off local HEAD).
func (s *ProjectService) SetProjectWorktreeBaseRef(id, ref string) (project.Project, error) {
	s.logger.Info("project.set-worktree-base-ref", "id", id, "ref", ref)
	p, err := s.projects.SetWorktreeBaseRef(id, ref)
	if err != nil {
		s.logger.Error("project.set-worktree-base-ref.failed", "id", id, "err", err)
	}
	return p, err
}

// DeleteProject removes a project and its bare clone from disk.
func (s *ProjectService) DeleteProject(id string) error {
	s.logger.Info("project.delete", "id", id)
	if err := s.projects.Delete(id); err != nil {
		s.logger.Error("project.delete.failed", "id", id, "err", err)
		return boardRejectionFor("project", id, err)
	}
	return nil
}

// ListWorktrees returns all git worktrees for the given project's bare clone.
func (s *ProjectService) ListWorktrees(projectID string) ([]project.Worktree, error) {
	if s.worktrees == nil {
		return nil, unavailableError("worktrees unavailable")
	}
	// context.Background(): Wails-bound method with no ctx.
	list, err := s.worktrees.List(context.Background(), projectID)
	// The project lookup fails first, so an unregistered id is a caller's
	// mistake rather than a server fault.
	return list, boardRejectionFor("project", projectID, err)
}

// OpenInTerminal opens a worktree path in a new Ghostty terminal tab.
func (s *ProjectService) OpenInTerminal(path string) error {
	if err := s.checkWorktreePath(path); err != nil {
		return err
	}
	return openDirInGhostty(path)
}

// OpenInEditor opens a worktree path in Zed.
func (s *ProjectService) OpenInEditor(path string) error {
	if err := s.checkWorktreePath(path); err != nil {
		return err
	}
	return exec.CommandContext(context.Background(), "zed", path).Start()
}

// checkWorktreePath rejects a path outside the worktrees directory as the
// caller's own mistake. ValidatePath's plain error would otherwise reach the
// HTTP mapper as a server fault and come back sanitized, hiding the reason.
func (s *ProjectService) checkWorktreePath(path string) error {
	if s.worktrees == nil {
		return unavailableError("worktrees unavailable")
	}
	if err := s.worktrees.ValidatePath(path); err != nil {
		return validationError(err.Error())
	}
	return nil
}
