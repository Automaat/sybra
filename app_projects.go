package main

import (
	"fmt"
	"os/exec"

	"github.com/Automaat/synapse/internal/executil"
	"github.com/Automaat/synapse/internal/project"
)

// ListProjects returns all registered projects.
func (a *App) ListProjects() ([]project.Project, error) {
	return a.projects.List()
}

// GetProject returns a single project by ID.
func (a *App) GetProject(id string) (project.Project, error) {
	return a.projects.Get(id)
}

// CreateProject clones a GitHub repo as a bare mirror and registers it.
func (a *App) CreateProject(url, ptype string) (project.Project, error) {
	a.logger.Info("project.create", "url", url, "type", ptype)
	p, err := a.projects.Create(url, project.ProjectType(ptype))
	if err != nil {
		a.logger.Error("project.create.failed", "url", url, "err", err)
		return p, err
	}
	a.logger.Info("project.created", "id", p.ID, "url", url)
	return p, nil
}

// UpdateProject changes the type (pet/work) of a registered project.
func (a *App) UpdateProject(id, ptype string) (project.Project, error) {
	a.logger.Info("project.update", "id", id, "type", ptype)
	p, err := a.projects.Update(id, project.ProjectType(ptype))
	if err != nil {
		a.logger.Error("project.update.failed", "id", id, "err", err)
		return p, err
	}
	return p, nil
}

// DeleteProject removes a project and its bare clone from disk.
func (a *App) DeleteProject(id string) error {
	a.logger.Info("project.delete", "id", id)
	if err := a.projects.Delete(id); err != nil {
		a.logger.Error("project.delete.failed", "id", id, "err", err)
		return err
	}
	return nil
}

// ListWorktrees returns all git worktrees for the given project's bare clone.
func (a *App) ListWorktrees(projectID string) ([]project.Worktree, error) {
	return a.worktrees.List(projectID)
}

// OpenInTerminal opens a worktree path in a new Ghostty terminal tab.
func (a *App) OpenInTerminal(path string) error {
	if err := a.worktrees.ValidatePath(path); err != nil {
		return err
	}
	return openDirInGhostty(path)
}

// OpenInEditor opens a worktree path in Zed.
func (a *App) OpenInEditor(path string) error {
	if err := a.worktrees.ValidatePath(path); err != nil {
		return err
	}
	return exec.Command("zed", path).Start()
}

func openDirInGhostty(dir string) error {
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Synapse:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'cd %s && exec zsh'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, executil.EscapeAppleScript(dir))
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}
