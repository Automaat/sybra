package main

import (
	"fmt"
	"log/slog"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/tmux"
)

// OrchestratorService exposes orchestrator session operations as Wails-bound methods.
type OrchestratorService struct {
	tmux   *tmux.Manager
	audit  *audit.Logger
	logger *slog.Logger
	emit   func(string, any)
}

// StartOrchestrator creates the orchestrator tmux session running claude.
func (s *OrchestratorService) StartOrchestrator() error {
	if s.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator already running")
	}
	if err := s.tmux.CreateSessionInDir(orchestratorSession, "claude", config.HomeDir()); err != nil {
		return fmt.Errorf("create orchestrator session: %w", err)
	}
	s.logger.Info("orchestrator.started")
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{Type: audit.EventOrchestratorStart})
	}
	s.emit(events.OrchestratorState, "running")
	return nil
}

// StopOrchestrator kills the orchestrator tmux session.
func (s *OrchestratorService) StopOrchestrator() error {
	if err := s.tmux.KillSession(orchestratorSession); err != nil {
		return fmt.Errorf("stop orchestrator: %w", err)
	}
	s.logger.Info("orchestrator.stopped")
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{Type: audit.EventOrchestratorStop})
	}
	s.emit(events.OrchestratorState, "stopped")
	return nil
}

// IsOrchestratorRunning reports whether the orchestrator tmux session exists.
func (s *OrchestratorService) IsOrchestratorRunning() bool {
	return s.tmux.SessionExists(orchestratorSession)
}

// CaptureOrchestratorPane returns the current terminal output of the orchestrator.
func (s *OrchestratorService) CaptureOrchestratorPane() (string, error) {
	if !s.tmux.SessionExists(orchestratorSession) {
		return "", fmt.Errorf("orchestrator not running")
	}
	return s.tmux.CapturePaneOutput(orchestratorSession)
}

// AttachOrchestrator opens the orchestrator tmux session in Ghostty.
func (s *OrchestratorService) AttachOrchestrator() error {
	if !s.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator not running")
	}
	return openTmuxInGhostty(orchestratorSession, "Orchestrator")
}
