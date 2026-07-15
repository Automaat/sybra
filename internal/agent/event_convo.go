package agent

import (
	"encoding/json"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

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
	ToolUseID string         `json:"toolUseId"`
	ToolName  string         `json:"toolName"`
	Input     map[string]any `json:"input"`
}

// ApprovalResponse carries the user's decision from the frontend.
type ApprovalResponse struct {
	ToolUseID string `json:"toolUseId"`
	Approved  bool   `json:"approved"`
}
