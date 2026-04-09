package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/events"
)

// ApprovalServer runs an HTTP server that handles PreToolUse hook requests
// from Claude CLI. When a tool needs approval, the hook POSTs to this server,
// which emits a Wails event to the frontend and blocks until the user responds.
type ApprovalServer struct {
	mu       sync.Mutex
	pending  map[string]chan ApprovalResponse // keyed by tool_use_id
	emit     EmitFunc
	logger   *slog.Logger
	server   *http.Server
	listener net.Listener
	agents   *Manager
}

// NewApprovalServer creates and starts the approval HTTP server on a random port.
func NewApprovalServer(emit EmitFunc, logger *slog.Logger) (*ApprovalServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	s := &ApprovalServer{
		pending:  make(map[string]chan ApprovalResponse),
		emit:     emit,
		logger:   logger,
		listener: listener,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/pre-tool-use", s.handlePreToolUse)

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("approval-server.serve", "err", err)
		}
	}()

	logger.Info("approval-server.start", "addr", listener.Addr().String())
	return s, nil
}

// Addr returns the listener address (e.g., "127.0.0.1:54321").
func (s *ApprovalServer) Addr() string {
	return s.listener.Addr().String()
}

// SetManager sets the agent manager reference for resolving agent IDs from tool_use_ids.
func (s *ApprovalServer) SetManager(m *Manager) {
	s.agents = m
}

// Shutdown gracefully stops the HTTP server.
func (s *ApprovalServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// hookInput matches the JSON the Claude CLI sends to PreToolUse hooks.
type hookInput struct {
	SessionID      string         `json:"session_id"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	ToolUseID      string         `json:"tool_use_id"`
	CWD            string         `json:"cwd"`
	PermissionMode string         `json:"permission_mode"`
}

// hookResponse is what we return to the Claude CLI hook.
type hookResponse struct {
	HookSpecificOutput hookOutput `json:"hookSpecificOutput"`
}

type hookOutput struct {
	HookEventName            string         `json:"hookEventName"`
	PermissionDecision       string         `json:"permissionDecision"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
}

func (s *ApprovalServer) handlePreToolUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input hookInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		s.logger.Error("approval-server.decode", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.logger.Info("approval-server.request", "tool", input.ToolName, "tool_use_id", input.ToolUseID)

	// Find the agent by session_id.
	agentID := s.findAgentBySession(input.SessionID)
	if agentID == "" {
		s.logger.Warn("approval-server.no-agent", "session_id", input.SessionID)
		// No agent found — auto-allow to avoid blocking.
		s.respondAllow(w, input.ToolInput)
		return
	}

	// Auto-approve safe read-only tools.
	if isSafeTool(input.ToolName) {
		s.respondAllow(w, input.ToolInput)
		return
	}

	// Create pending approval channel.
	ch := make(chan ApprovalResponse, 1)
	s.mu.Lock()
	s.pending[input.ToolUseID] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, input.ToolUseID)
		s.mu.Unlock()
	}()

	// Emit approval request to frontend.
	req := ApprovalRequest{
		ToolUseID: input.ToolUseID,
		ToolName:  input.ToolName,
		Input:     input.ToolInput,
	}
	s.emit(events.AgentApproval(agentID), req)

	// Update agent state to paused.
	if s.agents != nil {
		if a, err := s.agents.GetAgent(agentID); err == nil {
			a.SetState(StatePaused)
			s.emit(events.AgentState(agentID), a)
		}
	}

	// Block until user responds or context is cancelled.
	select {
	case resp := <-ch:
		if s.agents != nil {
			if a, err := s.agents.GetAgent(agentID); err == nil {
				a.SetState(StateRunning)
				s.emit(events.AgentState(agentID), a)
			}
		}
		if resp.Approved {
			s.respondAllow(w, input.ToolInput)
		} else {
			s.respondDeny(w, "User denied this action")
		}
	case <-r.Context().Done():
		s.respondAllow(w, input.ToolInput) // timeout → allow to avoid deadlock
	case <-time.After(5 * time.Minute):
		s.respondDeny(w, "Approval timed out")
	}
}

func (s *ApprovalServer) respondAllow(w http.ResponseWriter, input map[string]any) {
	writeHookResponse(w, hookOutput{
		HookEventName:      "PreToolUse",
		PermissionDecision: "allow",
		UpdatedInput:       input,
	})
}

func (s *ApprovalServer) respondDeny(w http.ResponseWriter, reason string) {
	writeHookResponse(w, hookOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	})
}

func writeHookResponse(w http.ResponseWriter, out hookOutput) {
	data, err := json.Marshal(hookResponse{HookSpecificOutput: out})
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// RespondApproval handles a user's approval/denial decision from the frontend.
func (s *ApprovalServer) RespondApproval(toolUseID string, approved bool) error {
	s.mu.Lock()
	ch, ok := s.pending[toolUseID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval for tool_use_id %s", toolUseID)
	}
	ch <- ApprovalResponse{ToolUseID: toolUseID, Approved: approved}
	return nil
}

func (s *ApprovalServer) findAgentBySession(sessionID string) string {
	if s.agents == nil {
		return ""
	}
	for _, a := range s.agents.ListAgents() {
		if a.GetSessionID() == sessionID && a.Mode == "interactive" {
			return a.ID
		}
	}
	return ""
}

func isSafeTool(name string) bool {
	safe := []string{"Read", "Glob", "Grep", "LSP", "WebSearch", "WebFetch"}
	lower := strings.ToLower(name)
	for _, s := range safe {
		if strings.EqualFold(s, name) || strings.HasPrefix(lower, "mcp__") {
			return true
		}
	}
	return false
}
