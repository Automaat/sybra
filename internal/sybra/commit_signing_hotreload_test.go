package sybra

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/review"
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
	svc := &ConfigService{}
	svc.subscribe(configSubscriber{
		Name:  "commit_signing",
		Paths: []string{"agent.commit_signing"},
		Apply: func(c config.Config) { gotPolicy = c.CommitSigning() },
	})

	var cfg config.Config
	cfg.Agent.CommitSigning = "never"
	// Callbacks read the live config rather than a captured snapshot.
	svc.cfg = &cfg
	svc.notifySubscribers([]string{"agent"}, cfg)

	if gotPolicy != "never" {
		t.Fatalf("a hot apply of \"agent\" did not reach the commit_signing subscriber: got %q, want never", gotPolicy)
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

	// The instruction sync writes to config.HomeDir(). Without this redirect
	// the test rewrites the operator's real ~/.sybra — the #1576 blast radius.
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("SYBRA_HOME", filepath.Join(isolated, "sybra"))

	projStore, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("new project store: %v", err)
	}
	reviewer := &review.Handler{}
	app := &App{
		ctx:      t.Context(),
		projects: projStore,
		reviewer: reviewer,
		logger:   slog.New(slog.DiscardHandler),
		repoDir:  repoRootForTest(t),
	}
	app.configSvc = &ConfigService{}
	app.cfg = &config.Config{}
	app.activeCfg.Store(app.cfg)
	app.wireConfigService()

	projStore.SetSigningPolicy(project.SigningAuto)
	workflow.SetDefaultCommitSignFlags("-s -S")
	// Establish the pre-reload baseline so the assertion below distinguishes
	// "the reload rewrote the file" from "the file was never synced".
	app.syncOrchestratorInstructions(t.Context(), project.SigningAuto)
	synced := filepath.Join(config.HomeDir(), "CLAUDE.md")
	baseline, err := os.ReadFile(synced)
	if err != nil {
		t.Fatalf("seed synced instructions: %v", err)
	}
	if !strings.Contains(string(baseline), "-s -S") {
		t.Fatalf("precondition: seeded instructions lack -s -S, assertion would be vacuous:\n%s", synced)
	}

	app.applyCommitSigning("never")

	if got := projStore.SigningPolicy(); got != project.SigningNever {
		t.Errorf("project store policy = %q, want never", got)
	}
	if got := workflow.DefaultCommitSignFlags(); got != "-s" {
		t.Errorf("workflow default commit flags = %q, want -s", got)
	}
	// The review dispatcher is the third sink: it caches the config snapshot
	// taken when the Handler was built, so the hook has to reach it too.
	if got := reviewer.SigningPolicy(); got != project.SigningNever {
		t.Errorf("review handler policy = %q, want never", got)
	}
	// The fourth sink: the orchestrator reads its flags out of this file, so a
	// reload that stops at the prompt leaves it on the old posture.
	body, err := os.ReadFile(synced)
	if err != nil {
		t.Fatalf("read synced instructions: %v", err)
	}
	if strings.Contains(string(body), "-s -S") {
		t.Errorf("synced instructions still say -S after reload to never:\n%s", synced)
	}
}

// repoRootForTest locates the repo checkout so the sync reads the real
// orchestrator instructions.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root not found")
	return ""
}
