package sybra

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/workflow"
)

// agent.commit_signing is registered hot, and both of its sinks cache the
// value outside the config struct. Without an explicit re-apply the reload
// reports Applied while dispatched prompts keep instructing the old flags and
// fresh clones keep the old floor — the store and UI show a change that never
// took effect.
func TestApplyAgentGuardrails_ReappliesCommitSigning(t *testing.T) {
	// A key-bearing host, so "never" and "auto" are distinguishable: on a
	// keyless box both resolve to -s and the assertion would be vacuous.
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	contents := "[user]\n\tname = Test\n\temail = t@example.invalid\n\tsigningkey = DEADBEEFDEADBEEF\n"
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	var gotPolicy string
	svc := &ConfigService{applyCommitSigning: func(raw string) { gotPolicy = raw }}

	var cfg config.Config
	cfg.Agent.CommitSigning = "never"
	svc.applyAgentGuardrails(cfg)

	if gotPolicy != "never" {
		t.Fatalf("applyAgentGuardrails did not re-apply commit signing: got %q, want never", gotPolicy)
	}
}

// The wired hook must drive both sinks, not just one: defect 1 was the stale
// workflow prompt, defect 2 the missing clone floor on projects created after
// the reload.
func TestCommitSigningHook_DrivesBothSinks(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	contents := "[user]\n\tname = Test\n\temail = t@example.invalid\n\tsigningkey = DEADBEEFDEADBEEF\n"
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	projStore, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}
	app := &App{ctx: t.Context(), projects: projStore}
	app.configSvc = &ConfigService{}
	app.cfg = &config.Config{}
	app.activeCfg.Store(app.cfg)
	app.wireConfigService()

	projStore.SetSigningPolicy(project.SigningAuto)
	workflow.SetDefaultCommitSignFlags("-s -S")

	app.configSvc.applyCommitSigning("never")

	if got := projStore.SigningPolicy(); got != project.SigningNever {
		t.Errorf("project store policy = %q, want never", got)
	}
	if got := workflow.DefaultCommitSignFlags(); got != "-s" {
		t.Errorf("workflow default commit flags = %q, want -s", got)
	}
}
