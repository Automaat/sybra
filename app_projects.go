package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/synapse/internal/project"
)

func (a *App) ListProjects() ([]project.Project, error) {
	return a.projects.List()
}

func (a *App) GetProject(id string) (project.Project, error) {
	return a.projects.Get(id)
}

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

func (a *App) UpdateProject(id, ptype string) (project.Project, error) {
	a.logger.Info("project.update", "id", id, "type", ptype)
	p, err := a.projects.Update(id, project.ProjectType(ptype))
	if err != nil {
		a.logger.Error("project.update.failed", "id", id, "err", err)
		return p, err
	}
	return p, nil
}

func (a *App) DeleteProject(id string) error {
	a.logger.Info("project.delete", "id", id)
	if err := a.projects.Delete(id); err != nil {
		a.logger.Error("project.delete.failed", "id", id, "err", err)
		return err
	}
	return nil
}

func (a *App) ListWorktrees(projectID string) ([]project.Worktree, error) {
	proj, err := a.projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	return project.ListWorktrees(proj.ClonePath)
}

func (a *App) OpenInTerminal(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(a.worktreesDir)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return openDirInGhostty(clean)
}

func (a *App) OpenInEditor(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(a.worktreesDir)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return exec.Command("zed", clean).Start()
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
end tell`, dir)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}
