package agent

import (
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

// MalformedToolCall records one malformed tool-call recovery outcome observed
// during a run. Outcome is "corrected" when Sybra queued an in-session
// correction turn, or "unrecoverable" when it fell back to workflow reschedule.
type MalformedToolCall struct {
	ToolUseID string
	Tool      string
	Outcome   string
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
	// BackgroundTaskIDs is populated (possibly to an empty, non-nil slice) for
	// a "system"/"background_tasks_changed" event: REPLACE-semantics snapshot
	// of every CLI background bash task still live after the change. See
	// Agent.SetBackgroundTaskIDs / Agent.EffectiveHangGrace.
	//
	// NOTE: omitempty collapses an empty-but-non-nil "all tasks cleared" slice
	// into the same absent-key wire form as a nil "no event seen" value. That
	// REPLACE-semantics distinction is preserved at the Go/ClaudeEvent level
	// but lost across this JSON boundary — fix the tag when a frontend consumer
	// that needs the cleared signal actually lands.
	BackgroundTaskIDs []string `json:"background_task_ids,omitempty"`
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
	// toolUses/toolResults preserve structured tool lifecycle metadata for the
	// live runner and reattach replay (foreground-command tracking). Unexported
	// so the NDJSON log and frontend wire format stay unchanged.
	toolUses    []ToolUseBlock
	toolResults []ToolResultBlock
	// permissionDenials carries auto-mode classifier denial records extracted
	// from this event's tool_result error blocks. Unexported so it is never
	// serialized; lives in-memory only. Populated for claude "user" events only.
	permissionDenials []PermissionDenial
	// parentToolUseID is the Claude Code `parent_tool_use_id` field, non-empty
	// when this event belongs to a forked subagent's turn rather than the
	// top-level conversation (see CLAUDE_CODE_FORK_SUBAGENT). Unexported so it
	// is never serialized; used only to exclude subagent chatter from the
	// top-level turn count.
	parentToolUseID string
}
