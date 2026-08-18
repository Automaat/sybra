package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/textutil"
)

const (
	toolFailureEventType = "sybra_tool_failure"
	toolFailureMaxField  = 2000
)

// ToolCallFailureRecord is a Sybra-authored diagnostic record persisted beside
// provider NDJSON so interrupted or denied tool calls can be audited later.
type ToolCallFailureRecord struct {
	Type              string    `json:"type"`
	Timestamp         time.Time `json:"timestamp"`
	AgentID           string    `json:"agent_id,omitempty"`
	TaskID            string    `json:"task_id,omitempty"`
	SessionID         string    `json:"session_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	ToolUseID         string    `json:"tool_use_id,omitempty"`
	ToolName          string    `json:"tool_name,omitempty"`
	ToolInputSummary  string    `json:"tool_input_summary,omitempty"`
	Source            string    `json:"source"`
	Reason            string    `json:"reason,omitempty"`
	TerminalReason    string    `json:"terminal_reason,omitempty"`
	ErrorType         string    `json:"error_type,omitempty"`
	ErrorStatus       int       `json:"error_status,omitempty"`
	ForegroundCommand string    `json:"foreground_command,omitempty"`
}

func (m *Manager) recordToolCallFailure(a *Agent, rec ToolCallFailureRecord) {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	rec.Type = toolFailureEventType
	if a != nil {
		if rec.AgentID == "" {
			rec.AgentID = a.ID
		}
		if rec.TaskID == "" {
			rec.TaskID = a.TaskID
		}
		if rec.SessionID == "" {
			rec.SessionID = a.GetSessionID()
		}
		if rec.Provider == "" {
			rec.Provider = a.GetProvider()
		}
		if rec.ForegroundCommand == "" {
			if cmd, _, ok := a.ActiveForegroundCommand(); ok {
				rec.ForegroundCommand = cmd
			}
		}
	}
	rec.ToolInputSummary = textutil.TruncateBytes(rec.ToolInputSummary, toolFailureMaxField, "...[truncated]")
	rec.Reason = textutil.TruncateBytes(rec.Reason, toolFailureMaxField, "...[truncated]")
	rec.ForegroundCommand = textutil.TruncateBytes(rec.ForegroundCommand, toolFailureMaxField, "...[truncated]")

	m.logger.Warn("agent.tool_failure",
		"id", rec.AgentID,
		"task_id", rec.TaskID,
		"tool", rec.ToolName,
		"tool_use_id", rec.ToolUseID,
		"source", rec.Source,
		"reason", rec.Reason,
		"terminal_reason", rec.TerminalReason)

	if a != nil {
		a.CountToolFailure()
		m.appendToolFailureRecord(a.GetLogPath(), rec)
	}
	m.appendToolFailureRecord(filepath.Join(m.logDir, "tool-failures.ndjson"), rec)
}

func (m *Manager) recordToolResultFailures(a *Agent, event StreamEvent) {
	for _, tr := range event.toolResults {
		if !tr.IsError {
			continue
		}
		toolName, input, _ := a.ToolUseByID(tr.ToolUseID)
		m.recordToolCallFailure(a, ToolCallFailureRecord{
			Timestamp:        event.Timestamp,
			ToolUseID:        tr.ToolUseID,
			ToolName:         toolName,
			ToolInputSummary: summarizeToolInput(input),
			Source:           "provider-tool-result-error",
			Reason:           tr.Content,
		})
	}
}

func (m *Manager) recordTerminalFailure(a *Agent, event StreamEvent) {
	if event.Type != "result" {
		return
	}
	source := ""
	switch {
	case event.TerminalReason == "aborted_tools":
		source = "stream-aborted-tools"
	case event.TerminalReason == "aborted_streaming":
		source = "stream-aborted-streaming"
	case resultSubtypeIsError(event.Subtype) || event.ErrorType != "" || event.ErrorStatus != 0:
		source = "provider-result-error"
	}
	if source == "" {
		return
	}
	m.recordToolCallFailure(a, ToolCallFailureRecord{
		Timestamp:      event.Timestamp,
		Source:         source,
		Reason:         event.Content,
		TerminalReason: event.TerminalReason,
		ErrorType:      event.ErrorType,
		ErrorStatus:    event.ErrorStatus,
	})
}

func (m *Manager) appendToolFailureRecord(path string, rec ToolCallFailureRecord) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.logger.Warn("agent.tool_failure.log_mkdir", "path", path, "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.logger.Warn("agent.tool_failure.log_open", "path", path, "err", err)
		return
	}
	defer func() { _ = f.Close() }()
	data, err := json.Marshal(rec)
	if err != nil {
		m.logger.Warn("agent.tool_failure.marshal", "err", err)
		return
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		m.logger.Warn("agent.tool_failure.log_write", "path", path, "err", err)
	}
}

func isToolFailureDiagnosticEvent(eventType string) bool {
	return eventType == toolFailureEventType
}

func summarizeToolInput(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "description", "url"} {
		if v, ok := input[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return textutil.TruncateBytes(s, toolFailureMaxField, "...[truncated]")
			}
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		return textutil.TruncateBytes(fmt.Sprint(input), toolFailureMaxField, "...[truncated]")
	}
	return textutil.TruncateBytes(string(data), toolFailureMaxField, "...[truncated]")
}

func logApprovalToolFailure(logger *slog.Logger, agentID, toolName, toolUseID, source, reason string) {
	logger.Warn("approval-server.tool_failure",
		"agent_id", agentID,
		"tool", toolName,
		"tool_use_id", toolUseID,
		"source", source,
		"reason", reason)
}
