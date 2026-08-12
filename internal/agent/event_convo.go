package agent

import (
	"encoding/json"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/textutil"
)

// claudeEventToConvoEvent converts a shared ClaudeEvent into a ConvoEvent for
// conversational mode. Tool result content is truncated to 2000 chars.
func claudeEventToConvoEvent(e ClaudeEvent) ConvoEvent {
	ev := ConvoEvent{
		Type:      e.Type,
		Subtype:   e.Subtype,
		SessionID: e.SessionID,
		Timestamp: time.Now().UTC(),
		Raw:       e.Raw,
	}
	switch e.Type {
	case "system":
		ev.BackgroundTaskIDs = e.BackgroundTaskIDs
	case "assistant":
		if e.Message != nil {
			ev.Text = e.Message.Text
			ev.ToolUses = e.Message.ToolUses
		}
	case "user":
		if e.Message != nil {
			results := make([]ToolResultBlock, len(e.Message.ToolResults))
			copy(results, e.Message.ToolResults)
			for i := range results {
				results[i].Content = textutil.TruncateBytes(results[i].Content, 2000, "...")
			}
			ev.ToolResults = results
		}
	case "result":
		if e.Result != nil {
			ev.Text = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.CostUSD = e.Result.CostUSD
			ev.InputTokens = e.Result.InputTokens
			ev.OutputTokens = e.Result.OutputTokens
			ev.CacheCreationInputTokens = e.Result.CacheCreationInputTokens
			ev.CacheReadInputTokens = e.Result.CacheReadInputTokens
			ev.ReasoningTokens = e.Result.ReasoningTokens
		}
	}
	return ev
}

// ConvoEvent is a rich event for conversational mode, preserving full tool
// call structure for the chat UI.
type ConvoEvent struct {
	Type                     string            `json:"type"`
	Subtype                  string            `json:"subtype,omitempty"`
	SessionID                string            `json:"sessionId,omitempty"`
	Text                     string            `json:"text,omitempty"`
	ToolUses                 []ToolUseBlock    `json:"toolUses,omitempty"`
	ToolResults              []ToolResultBlock `json:"toolResults,omitempty"`
	CostUSD                  float64           `json:"costUsd,omitempty"`
	InputTokens              int               `json:"inputTokens,omitempty"`
	OutputTokens             int               `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int               `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int               `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int               `json:"reasoningTokens,omitempty"`
	PremiumRequests          float64           `json:"premiumRequests,omitempty"`
	LimitSnapshot            *limits.Snapshot  `json:"limitSnapshot,omitempty"`
	IsPartial                bool              `json:"isPartial,omitempty"`
	Timestamp                time.Time         `json:"timestamp"`
	Raw                      json.RawMessage   `json:"raw,omitempty"`
	// ErrorType and ErrorStatus carry structured fields from the Anthropic error
	// envelope (e.g. "overloaded_error", 529) when subtype == "error".
	ErrorType   string `json:"errorType,omitempty"`
	ErrorStatus int    `json:"errorStatus,omitempty"`
	// BackgroundTaskIDs is populated (possibly to an empty, non-nil slice) for
	// a "system"/"background_tasks_changed" event: REPLACE-semantics snapshot
	// of every CLI background bash task (e.g. a `run_in_background` Bash call)
	// still live after the change. Mirrors ClaudeEvent.BackgroundTaskIDs.
	BackgroundTaskIDs []string `json:"backgroundTaskIds,omitempty"`
}

// ToolUseBlock represents a single tool call from the assistant.
type ToolUseBlock struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResultBlock represents the result of a tool execution.
type ToolResultBlock struct {
	ToolUseID string `json:"toolUseId"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
}

// ApprovalRequest is sent to the frontend when a tool needs user approval.
type ApprovalRequest struct {
	ToolUseID   string         `json:"toolUseId"`
	ToolName    string         `json:"toolName"`
	Input       map[string]any `json:"input"`
	Fingerprint string         `json:"fingerprint,omitempty"`
}

// ApprovalResponse carries the user's decision from the frontend.
type ApprovalResponse struct {
	ToolUseID string `json:"toolUseId"`
	Approved  bool   `json:"approved"`
}
