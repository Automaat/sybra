package sybra

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
)

var commitFlagDowngrader = strings.NewReplacer(
	"-s -S", "-s",
	"-sS", "-s",
	"-S -s", "-s",
)

// retiredSkillNames lists every skill Sybra used to install. Only these names
// are removed, because the directories they lived in also hold the operator's
// own skills.
var retiredSkillNames = []string{
	"adversarial-review",
	"claude-feature-adoption",
	"codex-feature-adoption",
	"fix-review-auto",
	"plan-critic",
	"plan-fork",
	"sybra-audit",
	"sybra-eval-report",
	"sybra-evaluate",
	"sybra-handoff",
	"sybra-monitor",
	"sybra-plan",
	"sybra-self-monitor",
	"sybra-tasks",
	"sybra-test",
	"sybra-triage",
	"why-human",
}

// syncOrchestratorInstructions copies the orchestrator brain into the Sybra home
// as CLAUDE.md and AGENTS.md. It needs a source checkout, which go.mod marks: a
// deployment without one keeps whatever is already there.
func (a *App) syncOrchestratorInstructions(ctx context.Context, signing project.SigningPolicy) {
	repoDir := a.repoDir
	if repoDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			a.logger.Error("orchestrator.instructions.no-repo", "err", err)
			return
		}
		repoDir = cwd
	}
	if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "orchestrator", "CLAUDE.md"))
	if err != nil {
		a.logger.Debug("orchestrator.instructions.read", "err", err)
		return
	}
	text := string(data)
	if !signing.SignsCommits(ctx) {
		text = commitFlagDowngrader.Replace(text)
	}
	home := config.HomeDir()
	if err := os.MkdirAll(home, 0o755); err != nil {
		a.logger.Error("orchestrator.instructions.mkdir", "dir", home, "err", err)
		return
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(text), 0o644); err != nil {
			a.logger.Error("orchestrator.instructions.write", "file", name, "err", err)
		}
	}
}

// removeInstalledSkills deletes the skills earlier versions installed, so
// providers that discover skills from disk stop offering them to agents.
func (a *App) removeInstalledSkills() {
	userHome, err := os.UserHomeDir()
	if err != nil {
		a.logger.Debug("skills.cleanup.no-user-home", "err", err)
		return
	}
	for _, dir := range installedSkillDirs(userHome) {
		removeRetiredSkills(dir, a.logger.Warn)
	}
}

func installedSkillDirs(userHome string) []string {
	return []string{
		filepath.Join(config.HomeDir(), ".claude", "skills"),
		filepath.Join(userHome, ".claude", "skills"),
		filepath.Join(userHome, ".codex", "skills"),
		filepath.Join(userHome, ".copilot", "skills"),
		filepath.Join(userHome, ".agents", "skills"),
	}
}

// removeRetiredSkills removes retired skills from dir. A symlink is a skill the
// operator manages themselves, and a directory with no SKILL.md is not a skill
// Sybra installed, so both stay.
func removeRetiredSkills(dir string, warn func(string, ...any)) {
	for _, name := range retiredSkillNames {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			warn("skills.cleanup.remove", "path", path, "err", err)
		}
	}
}
