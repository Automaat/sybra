package agent

import (
	"encoding/json"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

// PermissionDenial records a single auto-mode classifier denial observed during
// a headless run. Populated from tool_result error blocks that match the Claude
// Code auto-mode classifier denial marker.
type PermissionDenial struct {
	ToolUseID string
	Reason    string
}

// PlanStep represents a single item from a TodoWrite tool call.
type PlanStep struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending", "in_progress", "completed"
}

type StreamEvent struct {
	Type                     string  `json:"type"`
	Content                  string  `json:"content,omitempty"`
	SessionID                string  `json:"session_id,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
	InputTokens              int     `json:"input_tokens,omitempty"`
	OutputTokens             int     `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int     `json:"reasoning_tokens,omitempty"`
	// PremiumRequests is Copilot's per-result billing-unit count (result event)
	// or 0 for claude/codex.
	PremiumRequests float64   `json:"premium_requests,omitempty"`
	Subtype         string    `json:"subtype,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	// ErrorType and ErrorStatus carry structured fields from the Anthropic error
	// envelope (e.g. "overloaded_error", 529) when subtype == "error".
	ErrorType   string `json:"error_type,omitempty"`
	ErrorStatus int    `json:"error_status,omitempty"`
	// PlanSteps is populated when the assistant calls TodoWrite; contains the
	// latest snapshot of the agent's todo list at this point in the stream.
	PlanSteps []PlanStep `json:"plan_steps,omitempty"`
	// PluginErrors carries plugin load failures surfaced by the init event.
	PluginErrors []string `json:"plugin_errors,omitempty"`
	// ToolCalls is the number of tool_use blocks in this event: all tool uses
	// in a Claude assistant turn, or a single Codex tool_use. The runner
	// accumulates these into Agent.ToolCalls.
	ToolCalls int `json:"tool_calls,omitempty"`
	// LimitSnapshot carries provider quota status emitted by CLIs such as
	// Codex. It is forwarded to the limits ledger and not rendered as normal
	// assistant output.
	LimitSnapshot *limits.Snapshot `json:"limit_snapshot,omitempty"`
	// toolSig is a canonical fingerprint of this event's tool calls (name +
	// input), used by the watchdog's real-time loop detector to spot an agent
	// repeating the same call. Unexported so it is never serialized to the
	// NDJSON log or emitted to the frontend; it lives only in memory.
	toolSig string
	// permissionDenials carries auto-mode classifier denial records extracted
	// from this event's tool_result error blocks. Unexported so it is never
	// serialized; lives in-memory only. Populated for claude "user" events only.
	permissionDenials []PermissionDenial
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

// ErrorEvent is the payload emitted on agent:error:{id}.
type ErrorEvent struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// PluginErrorsEvent is emitted on agent:plugin_errors:{id} when the init event
// carries plugin load failures.
type PluginErrorsEvent struct {
	Errors []string `json:"errors"`
}

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
