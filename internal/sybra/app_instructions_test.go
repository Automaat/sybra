package sybra

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/loopagent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/prompts"
	"github.com/Automaat/sybra/internal/providerid"
)

func TestRemoveRetiredSkillsKeepsWhatSybraDidNotInstall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkill := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill("plan-critic")
	writeSkill("my-own-skill")
	if err := os.MkdirAll(filepath.Join(dir, "sybra-plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	dotfiles := t.TempDir()
	if err := os.Symlink(dotfiles, filepath.Join(dir, "sybra-tasks")); err != nil {
		t.Fatal(err)
	}

	removeRetiredSkills(dir, func(string, ...any) {})

	if _, err := os.Stat(filepath.Join(dir, "plan-critic")); !os.IsNotExist(err) {
		t.Errorf("retired skill still present: %v", err)
	}
	for _, kept := range []string{"my-own-skill", "sybra-plan", "sybra-tasks"} {
		if _, err := os.Lstat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	if _, err := os.Stat(dotfiles); err != nil {
		t.Errorf("symlink target was removed: %v", err)
	}
}

func TestSyncOrchestratorInstructionsNeedsACheckout(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "orchestrator", "CLAUDE.md"), []byte("git commit -s -S"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{repoDir: repo, logger: slog.New(slog.DiscardHandler)}

	app.syncOrchestratorInstructions(t.Context(), project.SigningAuto)
	if _, err := os.Stat(filepath.Join(config.HomeDir(), "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("synced without a go.mod: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.syncOrchestratorInstructions(t.Context(), project.SigningNever)
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		got, err := os.ReadFile(filepath.Join(config.HomeDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "git commit -s" {
			t.Errorf("%s = %q, want downgraded commit flags", name, got)
		}
	}
}

func TestSeedDefaultLoopAgentsMigratesOnlyTheLegacyPrompt(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		legacySelfMonitorPrompt: prompts.MustGet(prompts.SelfMonitor),
		"operator edited this":  "operator edited this",
	}
	for saved, want := range cases {
		t.Run(saved, func(t *testing.T) {
			t.Parallel()
			store, err := loopagent.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Create(t.Context(), loopagent.LoopAgent{Name: "sybra-self-monitor", Prompt: saved, IntervalSec: 21600, Provider: providerid.Claude}); err != nil {
				t.Fatal(err)
			}
			app := &App{loopAgents: store, logger: slog.New(slog.DiscardHandler)}

			app.seedDefaultLoopAgents(t.Context())

			got, ok := store.FindByName(t.Context(), "sybra-self-monitor")
			if !ok {
				t.Fatal("loop agent missing")
			}
			if got.Prompt != want {
				t.Errorf("prompt = %.60q, want %.60q", got.Prompt, want)
			}
		})
	}
}
