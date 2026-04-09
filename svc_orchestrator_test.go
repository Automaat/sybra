package main

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/tmux"
)

func requireTmuxMain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
}

func TestOrchestratorService_StartOrchestrator_Codex(t *testing.T) {
	requireTmuxMain(t)
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	svc := &OrchestratorService{
		tmux:   tmux.NewManager(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		emit:   func(string, any) {},
		cfg:    &config.Config{Agent: config.AgentDefaults{Provider: "codex"}},
	}
	argsLog := filepath.Join(t.TempDir(), "codex-args.log")
	t.Setenv("FAKE_CODEX_ARGS_LOG", argsLog)
	_ = svc.tmux.KillSession(orchestratorSession)
	t.Cleanup(func() { _ = svc.StopOrchestrator() })

	if err := svc.StartOrchestrator(); err != nil {
		t.Fatal(err)
	}

	cmd, err := svc.tmux.PaneCommand(orchestratorSession)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "codex") {
		t.Fatalf("pane command = %q, want command containing codex", cmd)
	}
	waitFor(t, 2*time.Second, "fake codex invoked", func() bool {
		_, err := os.Stat(argsLog)
		return err == nil
	})
}
