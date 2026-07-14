package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/Automaat/sybra/internal/limits"
)

// ClaudeMessage holds parsed content blocks from assistant or user messages.
type ClaudeMessage struct {
	Role        string            // "assistant" or "user"
	Text        string            // joined text blocks only
	ToolUses    []ToolUseBlock    // tool_use blocks (structured)
	ToolResults []ToolResultBlock // tool_result blocks (structured, untruncated)
}

// ClaudeResult holds fields from "result" events.
//
// Cache tokens dominate input volume in long agent runs (cache reads typically
// 100–10000× larger than uncached `InputTokens`). Captured separately so the
// stats UI can reflect actual API consumption — `InputTokens` alone is just
// the small uncached delta and looks "too small" without these.
type ClaudeResult struct {
	Subtype                  string
	Text                     string
	SessionID                string
	CostUSD                  float64
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ReasoningTokens          int
	// ErrorType and ErrorStatus carry structured error info when available.
	// Codex maps these from its error envelope; Claude maps `error` and
	// `api_error_status` from result events that set `is_error`.
	ErrorType   string
	ErrorStatus int
	// PremiumRequests is Copilot's billing unit (AI credits), mapped from the
	// result event's `usage.premiumRequests`. Sybra estimates USD from this
	// separately; always 0 for claude/codex.
	PremiumRequests float64
}

// ClaudeEvent is the shared envelope for all Claude stream-json events.
type ClaudeEvent struct {
	Type         string
	Subtype      string
	SessionID    string
	PluginErrors []string
	Raw          json.RawMessage // independent copy, never aliased to scanner buffer
	Message      *ClaudeMessage
	Result       *ClaudeResult
	// ParentToolUseID is Claude Code's `parent_tool_use_id` field, non-empty
	// when the event belongs to a forked subagent's turn (CLAUDE_CODE_FORK_SUBAGENT)
	// rather than the top-level conversation.
	ParentToolUseID string
	// BackgroundTaskIDs is populated (possibly to an empty, non-nil slice) for
	// a "system"/"background_tasks_changed" event: REPLACE-semantics snapshot
	// of every CLI background bash task (e.g. a `run_in_background` Bash call)
	// still live after the change.
	BackgroundTaskIDs []string
}

// CodexEvent is the shared envelope for all Codex stream-json events.
type CodexEvent struct {
	Type      string
	Subtype   string
	SessionID string
	Raw       json.RawMessage
	Message   *ClaudeMessage // reuses ClaudeMessage for shared content structure
	Result    *ClaudeResult
	Limits    *limits.Snapshot
}

// CopilotEvent is the shared envelope for all GitHub Copilot CLI stream-json
// events (`copilot --output-format json`). Reuses ClaudeMessage/ClaudeResult
// for the shared content/result structure.
type CopilotEvent struct {
	Type      string
	Subtype   string
	SessionID string
	// OutputTokens carries the per-message output-token count Copilot reports
	// on each assistant.message (it does not total them on the result event).
	OutputTokens int
	Raw          json.RawMessage
	Message      *ClaudeMessage
	Result       *ClaudeResult
}

// OpenCodeEvent is the shared envelope for OpenCode JSON output
// (`opencode run --format json`). OpenCode supports many providers, so the
// parser is intentionally tolerant of both OpenCode-native event names and the
// assistant/tool/result shapes used by the other CLIs.
type OpenCodeEvent struct {
	Type         string
	Subtype      string
	SessionID    string
	OutputTokens int
	Raw          json.RawMessage
	Message      *ClaudeMessage
	Result       *ClaudeResult
}

type claudeEnvelope struct {
	Type            string                `json:"type"`
	Subtype         string                `json:"subtype"`
	SessionID       string                `json:"session_id"`
	PluginErrors    []string              `json:"plugin_errors,omitempty"`
	ParentToolUseID string                `json:"parent_tool_use_id,omitempty"`
	Message         *claudeMessagePayload `json:"message"`
	Result          string                `json:"result"`
	TotalCostUSD    float64               `json:"total_cost_usd"`
	IsError         bool                  `json:"is_error"`
	APIStatus       int                   `json:"api_error_status"`
	Error           string                `json:"error"`
	// Real Claude Code result events nest token counts under `usage` (per
	// platform.claude.com/docs/en/agent-sdk/headless and verified against
	// captured agent NDJSON). Earlier root-level `total_*_tokens` fields are
	// kept as a fallback for fixtures that still use them.
	Usage             *claudeUsage `json:"usage"`
	TotalInputTokens  int          `json:"total_input_tokens"`
	TotalOutputTokens int          `json:"total_output_tokens"`
	// Tasks is populated on a "system"/"background_tasks_changed" event: the
	// full set of currently-live background bash tasks (REPLACE semantics).
	Tasks []claudeBackgroundTaskRef `json:"tasks,omitempty"`
}

// claudeBackgroundTaskRef is one entry of a "background_tasks_changed"
// event's `tasks` array — a live CLI background bash task (e.g. a
// `run_in_background` Bash call). Only TaskID is consumed today;
// TaskType/Description are kept for future diagnostics.
type claudeBackgroundTaskRef struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type"`
	Description string `json:"description"`
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type claudeMessagePayload struct {
	Content []claudeContentBlock `json:"content"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeTextBlock struct {
	Text string `json:"text"`
}

type codexEnvelope struct {
	Type      string      `json:"type"`
	ThreadID  string      `json:"thread_id"`
	Message   string      `json:"message"`
	ErrorType string      `json:"error_type"`
	Code      int         `json:"code"`
	Item      *codexItem  `json:"item"`
	Usage     *codexUsage `json:"usage"`
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	// Real Codex emits `reasoning_output_tokens`; `reasoning_tokens` kept as
	// fallback for any legacy/test fixture using the shorter name.
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	ReasoningTokens       int `json:"reasoning_tokens"`
}

// copilotEnvelope is the shared wrapper for every `copilot --output-format
// json` line. Most events nest their payload under `data`; the terminal
// `result` event carries its fields at the top level. `ephemeral:true` marks
// streaming/status lines (token deltas, MCP/skill load notices) that the
// headless model drops.
type copilotEnvelope struct {
	Type      string        `json:"type"`
	Ephemeral bool          `json:"ephemeral"`
	SessionID string        `json:"sessionId"` // result event only
	ExitCode  int           `json:"exitCode"`  // result event only
	Data      *copilotData  `json:"data"`
	Usage     *copilotUsage `json:"usage"` // result event only
}

type copilotData struct {
	Model        string               `json:"model"`
	Content      string               `json:"content"`
	OutputTokens int                  `json:"outputTokens"`
	ToolRequests []copilotToolRequest `json:"toolRequests"`
	ToolName     string               `json:"toolName"`
	ToolCallID   string               `json:"toolCallId"`
	Arguments    *copilotToolArgs     `json:"arguments"`
	Success      *bool                `json:"success"`
	Result       *copilotToolResult   `json:"result"`
}

type copilotToolRequest struct {
	ToolCallID string           `json:"toolCallId"`
	Name       string           `json:"name"`
	Arguments  *copilotToolArgs `json:"arguments"`
}

type copilotToolArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type copilotToolResult struct {
	// Content is RawMessage, not string: Copilot emits a string today, but a
	// structured (array/object) content would otherwise fail to unmarshal and
	// drop the entire tool.execution_complete event (losing the success/error
	// signal), not just the body. copilotResultContent renders it defensively.
	Content json.RawMessage `json:"content"`
}

type copilotUsage struct {
	PremiumRequests float64 `json:"premiumRequests"`
}

type opencodeEnvelope struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"sessionId"`
	SessionIDSnake string          `json:"session_id"`
	ExitCode       int             `json:"exitCode"`
	Code           int             `json:"code"`
	ErrorType      string          `json:"errorType"`
	ErrorTypeSnake string          `json:"error_type"`
	Error          string          `json:"error"`
	Message        string          `json:"message"`
	Content        string          `json:"content"`
	Text           string          `json:"text"`
	Data           json.RawMessage `json:"data"`
	Usage          *opencodeUsage  `json:"usage"`
}

type opencodeUsage struct {
	InputTokens               int     `json:"inputTokens"`
	InputTokensSnake          int     `json:"input_tokens"`
	OutputTokens              int     `json:"outputTokens"`
	OutputTokensSnake         int     `json:"output_tokens"`
	CacheReadInputTokens      int     `json:"cacheReadInputTokens"`
	CacheReadInputTokensSnake int     `json:"cache_read_input_tokens"`
	ReasoningTokens           int     `json:"reasoningTokens"`
	ReasoningTokensSnake      int     `json:"reasoning_tokens"`
	CostUSD                   float64 `json:"costUSD"`
	CostUSDSnake              float64 `json:"cost_usd"`
}

type opencodeData struct {
	ID           string             `json:"id"`
	Content      string             `json:"content"`
	Text         string             `json:"text"`
	Message      string             `json:"message"`
	OutputTokens int                `json:"outputTokens"`
	ToolCallID   string             `json:"toolCallId"`
	ToolID       string             `json:"toolId"`
	ToolName     string             `json:"toolName"`
	Name         string             `json:"name"`
	Arguments    json.RawMessage    `json:"arguments"`
	Input        json.RawMessage    `json:"input"`
	Success      *bool              `json:"success"`
	Error        string             `json:"error"`
	Result       json.RawMessage    `json:"result"`
	ToolRequests []opencodeToolCall `json:"toolRequests"`
	ToolCalls    []opencodeToolCall `json:"toolCalls"`
}

type opencodeToolCall struct {
	ID         string          `json:"id"`
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	ToolName   string          `json:"toolName"`
	Arguments  json.RawMessage `json:"arguments"`
	Input      json.RawMessage `json:"input"`
}

// ParseCopilotLine parses one line of GitHub Copilot CLI stream-json output.
// Ephemeral and structural lines collapse to a zero-Type CopilotEvent that
// callers skip. The returned Raw is an independent copy safe to keep after the
// scanner buffer is reused.
func ParseCopilotLine(line []byte) (CopilotEvent, error) {
	var raw copilotEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return CopilotEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	rawCopy := copyRaw(line)
	// Drop streaming deltas and session/MCP/skill status noise outright.
	if raw.Ephemeral {
		return CopilotEvent{Raw: rawCopy}, nil
	}

	switch raw.Type {
	case "assistant.message":
		// Non-ephemeral, final assistant turn message: joined text plus any
		// tool requests. Tool execution itself arrives as separate tool.* events.
		msg := &ClaudeMessage{Role: "assistant"}
		var outTokens int
		if raw.Data != nil {
			msg.Text = raw.Data.Content
			outTokens = raw.Data.OutputTokens
			for _, tr := range raw.Data.ToolRequests {
				msg.ToolUses = append(msg.ToolUses, ToolUseBlock{
					ID:    tr.ToolCallID,
					Name:  copilotToolDisplayName(tr.Name),
					Input: copilotToolInput(tr.Arguments),
				})
			}
		}
		return CopilotEvent{Type: "assistant", OutputTokens: outTokens, Raw: rawCopy, Message: msg}, nil

	case "tool.execution_start":
		if raw.Data == nil {
			return CopilotEvent{Raw: rawCopy}, nil
		}
		return CopilotEvent{
			Type: "tool_use",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "assistant",
				ToolUses: []ToolUseBlock{{
					ID:    raw.Data.ToolCallID,
					Name:  copilotToolDisplayName(raw.Data.ToolName),
					Input: copilotToolInput(raw.Data.Arguments),
				}},
			},
		}, nil

	case "tool.execution_complete":
		if raw.Data == nil {
			return CopilotEvent{Raw: rawCopy}, nil
		}
		isErr := raw.Data.Success != nil && !*raw.Data.Success
		content := ""
		if raw.Data.Result != nil {
			content = copilotResultContent(raw.Data.Result.Content)
		}
		return CopilotEvent{
			Type: "tool_result",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "user",
				ToolResults: []ToolResultBlock{{
					ToolUseID: raw.Data.ToolCallID,
					Content:   content,
					IsError:   isErr,
				}},
			},
		}, nil

	case "result":
		r := &ClaudeResult{SessionID: raw.SessionID}
		if raw.Usage != nil {
			r.PremiumRequests = raw.Usage.PremiumRequests
		}
		// A non-zero exit code means the run failed. Mark the result as an error
		// (mirroring codex's error mapping) so reattach/restart completion does
		// not treat a crashed copilot run as a clean success, and so the error
		// classifier sees a populated sample.
		subtype := ""
		if raw.ExitCode != 0 {
			subtype = "error"
			r.Subtype = "error"
			r.ErrorStatus = raw.ExitCode
			r.ErrorType = "nonzero_exit"
		}
		return CopilotEvent{Type: "result", Subtype: subtype, SessionID: raw.SessionID, Raw: rawCopy, Result: r}, nil

	default:
		// user.message, assistant.turn_start/turn_end, assistant.reasoning,
		// function, and any unknown non-ephemeral type carry no displayable
		// content for the headless model — skip them.
		return CopilotEvent{Raw: rawCopy}, nil
	}
}

// ParseOpenCodeLine parses one line of OpenCode JSON output. Unknown structural
// lines collapse to a zero-Type event so callers can keep streaming across new
// OpenCode event types without failing the whole run.
func ParseOpenCodeLine(line []byte) (OpenCodeEvent, error) {
	var raw opencodeEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return OpenCodeEvent{}, fmt.Errorf("unmarshal: %w", err)
	}
	rawCopy := copyRaw(line)
	sessionID := firstNonEmpty(raw.SessionID, raw.SessionIDSnake)
	eventType := strings.ToLower(strings.TrimSpace(raw.Type))

	switch eventType {
	case "assistant.message", "assistant", "message", "agent_message", "message.updated":
		msg := &ClaudeMessage{Role: "assistant", Text: firstNonEmpty(raw.Content, raw.Text, raw.Message)}
		outTokens := 0
		if len(raw.Data) > 0 {
			data := parseOpenCodeData(raw.Data)
			if msg.Text == "" {
				msg.Text = firstNonEmpty(data.Content, data.Text, data.Message)
			}
			outTokens = data.OutputTokens
			msg.ToolUses = append(msg.ToolUses, opencodeToolCalls(data.ToolRequests)...)
			msg.ToolUses = append(msg.ToolUses, opencodeToolCalls(data.ToolCalls)...)
		}
		if raw.Usage != nil && outTokens == 0 {
			outTokens = raw.Usage.outputTokens()
		}
		return OpenCodeEvent{Type: "assistant", SessionID: sessionID, OutputTokens: outTokens, Raw: rawCopy, Message: msg}, nil

	case "tool.execution_start", "tool.started", "tool.start", "tool_use", "tool.call":
		data := parseOpenCodeData(raw.Data)
		tool := ToolUseBlock{
			ID:    firstNonEmpty(data.ToolCallID, data.ToolID, data.ID),
			Name:  opencodeToolDisplayName(firstNonEmpty(data.ToolName, data.Name)),
			Input: opencodeInputMap(firstRaw(data.Arguments, data.Input)),
		}
		return OpenCodeEvent{Type: "tool_use", SessionID: sessionID, Raw: rawCopy, Message: &ClaudeMessage{
			Role:     "assistant",
			ToolUses: []ToolUseBlock{tool},
		}}, nil

	case "tool.execution_complete", "tool.completed", "tool.complete", "tool_result", "tool.result":
		data := parseOpenCodeData(raw.Data)
		success := data.Success == nil || *data.Success
		content := firstNonEmpty(data.Error, opencodeRawContent(data.Result), raw.Content, raw.Text, raw.Message)
		return OpenCodeEvent{Type: "tool_result", SessionID: sessionID, Raw: rawCopy, Message: &ClaudeMessage{
			Role: "user",
			ToolResults: []ToolResultBlock{{
				ToolUseID: firstNonEmpty(data.ToolCallID, data.ToolID, data.ID),
				Content:   content,
				IsError:   !success || data.Error != "",
			}},
		}}, nil

	case "result", "session.idle", "session.completed", "turn.completed", "run.completed":
		r := &ClaudeResult{
			SessionID:   sessionID,
			Text:        firstNonEmpty(raw.Content, raw.Text, raw.Message),
			ErrorType:   firstNonEmpty(raw.ErrorType, raw.ErrorTypeSnake),
			ErrorStatus: firstNonZero(raw.Code, raw.ExitCode),
		}
		if raw.Usage != nil {
			r.InputTokens = raw.Usage.inputTokens()
			r.OutputTokens = raw.Usage.outputTokens()
			r.CacheReadInputTokens = raw.Usage.cacheReadInputTokens()
			r.ReasoningTokens = raw.Usage.reasoningTokens()
			r.CostUSD = raw.Usage.costUSD()
		}
		subtype := ""
		if raw.Subtype != "" {
			subtype = raw.Subtype
		} else if r.ErrorStatus != 0 || r.ErrorType != "" || raw.Error != "" {
			subtype = "error"
			if r.Text == "" {
				r.Text = raw.Error
			}
		}
		r.Subtype = subtype
		return OpenCodeEvent{Type: "result", Subtype: subtype, SessionID: sessionID, Raw: rawCopy, Result: r}, nil

	case "error":
		status := firstNonZero(raw.Code, raw.ExitCode)
		return OpenCodeEvent{
			Type:    "result",
			Subtype: "error",
			Raw:     rawCopy,
			Result: &ClaudeResult{
				Subtype:     "error",
				SessionID:   sessionID,
				Text:        firstNonEmpty(raw.Error, raw.Message, raw.Content, raw.Text),
				ErrorType:   firstNonEmpty(raw.ErrorType, raw.ErrorTypeSnake, "error"),
				ErrorStatus: status,
			},
		}, nil

	case "session.created", "session.updated", "session":
		return OpenCodeEvent{Type: "init", SessionID: sessionID, Raw: rawCopy}, nil

	default:
		return OpenCodeEvent{Raw: rawCopy}, nil
	}
}

func parseOpenCodeData(raw json.RawMessage) opencodeData {
	var data opencodeData
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &data)
	}
	return data
}

func opencodeToolCalls(calls []opencodeToolCall) []ToolUseBlock {
	out := make([]ToolUseBlock, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolUseBlock{
			ID:    firstNonEmpty(c.ToolCallID, c.ID),
			Name:  opencodeToolDisplayName(firstNonEmpty(c.ToolName, c.Name)),
			Input: opencodeInputMap(firstRaw(c.Arguments, c.Input)),
		})
	}
	return out
}

func opencodeInputMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return out
	}
	if json.Unmarshal(raw, &out) == nil {
		return out
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		out["input"] = s
		return out
	}
	out["input"] = string(raw)
	return out
}

func opencodeRawContent(raw json.RawMessage) string {
	return copilotResultContent(raw)
}

func opencodeToolDisplayName(name string) string {
	if strings.EqualFold(name, "bash") || strings.EqualFold(name, "shell") {
		return "Bash"
	}
	if name == "" {
		return "Tool"
	}
	return name
}

func firstRaw(items ...json.RawMessage) json.RawMessage {
	for _, item := range items {
		if len(item) > 0 && !bytes.Equal(item, []byte("null")) {
			return item
		}
	}
	return nil
}

func firstNonEmpty(items ...string) string {
	for _, item := range items {
		if item != "" {
			return item
		}
	}
	return ""
}

func firstNonZero(items ...int) int {
	for _, item := range items {
		if item != 0 {
			return item
		}
	}
	return 0
}

func (u opencodeUsage) inputTokens() int {
	if u.InputTokens != 0 {
		return u.InputTokens
	}
	return u.InputTokensSnake
}

func (u opencodeUsage) outputTokens() int {
	if u.OutputTokens != 0 {
		return u.OutputTokens
	}
	return u.OutputTokensSnake
}

func (u opencodeUsage) cacheReadInputTokens() int {
	if u.CacheReadInputTokens != 0 {
		return u.CacheReadInputTokens
	}
	return u.CacheReadInputTokensSnake
}

func (u opencodeUsage) reasoningTokens() int {
	if u.ReasoningTokens != 0 {
		return u.ReasoningTokens
	}
	return u.ReasoningTokensSnake
}

func (u opencodeUsage) costUSD() float64 {
	if u.CostUSD != 0 {
		return u.CostUSD
	}
	return u.CostUSDSnake
}

// copilotToolInput builds the ToolUseBlock.Input map from a Copilot tool's
// arguments. Copilot's shell tool carries command/description; other tools
// carry different keys not yet modeled, so this captures what is known.
func copilotToolInput(args *copilotToolArgs) map[string]any {
	input := map[string]any{}
	if args != nil {
		if args.Command != "" {
			input["command"] = args.Command
		}
		if args.Description != "" {
			input["description"] = args.Description
		}
	}
	return input
}

// copilotResultContent renders a Copilot tool result's `content` defensively.
// Copilot emits a plain string today; tolerate an array of {text} blocks or any
// other shape so a structured result never drops the whole event.
func copilotResultContent(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		var parts []claudeTextBlock
		if json.Unmarshal(raw, &parts) == nil {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
			return strings.Join(texts, "\n")
		}
	}
	return string(raw)
}

// copilotToolDisplayName normalizes a Copilot tool name for display. Copilot's
// shell tool is "bash"; surface it as "Bash" so it renders the same as the
// Codex command-execution tool. Other names pass through unchanged.
func copilotToolDisplayName(name string) string {
	if strings.EqualFold(name, "bash") || strings.EqualFold(name, "shell") {
		return "Bash"
	}
	return name
}

// ParseClaudeLine parses one line of Claude stream-json output.
// The returned ClaudeEvent.Raw is an independent copy safe to keep after the
// scanner buffer is reused.
func ParseClaudeLine(line []byte) (ClaudeEvent, error) {
	var raw claudeEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return ClaudeEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	event := ClaudeEvent{
		Type:            raw.Type,
		Subtype:         raw.Subtype,
		Raw:             copyRaw(line),
		ParentToolUseID: raw.ParentToolUseID,
	}

	switch raw.Type {
	case "system", "init":
		event.SessionID = raw.SessionID
		event.PluginErrors = raw.PluginErrors
		if raw.Subtype == "background_tasks_changed" {
			ids := make([]string, len(raw.Tasks))
			for i, t := range raw.Tasks {
				ids[i] = t.TaskID
			}
			event.BackgroundTaskIDs = ids
		}

	case "assistant":
		if raw.Message != nil {
			m := extractAssistantContentTyped(raw.Message)
			event.Message = &m
		}
		event.SessionID = raw.SessionID

	case "user":
		if raw.Message != nil {
			results := extractToolResultsTyped(raw.Message)
			event.Message = &ClaudeMessage{Role: "user", ToolResults: results}
		}

	case "result":
		r := extractResultFieldsTyped(raw)
		event.Result = &r
		event.SessionID = r.SessionID

	default:
		// rate_limit_event, etc — keep type only
	}

	return event, nil
}

// ParseCodexLine parses one line of Codex stream-json output.
// The returned CodexEvent.Raw is an independent copy safe to keep after the
// scanner buffer is reused.
func ParseCodexLine(line []byte) (CodexEvent, error) {
	var raw codexEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return CodexEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	rawCopy := copyRaw(line)
	if snapshot, ok := limits.SnapshotFromCodexRaw(line, limits.SourceStream); ok && snapshot.Provider != "" {
		return CodexEvent{Type: "usage", Raw: rawCopy, Limits: &snapshot}, nil
	}

	switch raw.Type {
	case "thread.started":
		return CodexEvent{Type: "init", SessionID: raw.ThreadID, Raw: rawCopy}, nil

	case "turn.started":
		return CodexEvent{Type: "init", Raw: rawCopy}, nil

	case "error":
		return CodexEvent{
			Type:    "result",
			Subtype: "error",
			Raw:     rawCopy,
			Result: &ClaudeResult{
				Subtype:     "error",
				Text:        raw.Message,
				ErrorType:   raw.ErrorType,
				ErrorStatus: raw.Code,
			},
		}, nil

	case "turn.completed":
		var r ClaudeResult
		if raw.Usage != nil {
			r.InputTokens = raw.Usage.InputTokens
			r.OutputTokens = raw.Usage.OutputTokens
			// Codex `cached_input_tokens` is a subset of `input_tokens` (gross
			// total). Map to CacheReadInputTokens so the stats UI can break
			// down billed vs cache-hit tokens with the same column shape used
			// for Claude.
			r.CacheReadInputTokens = raw.Usage.CachedInputTokens
			// Real codex emits `reasoning_output_tokens`; older fixtures use
			// the shorter `reasoning_tokens`. Prefer the canonical field and
			// fall back so tests added under the legacy name still pass.
			if raw.Usage.ReasoningOutputTokens != 0 {
				r.ReasoningTokens = raw.Usage.ReasoningOutputTokens
			} else {
				r.ReasoningTokens = raw.Usage.ReasoningTokens
			}
		}
		return CodexEvent{Type: "result", Raw: rawCopy, Result: &r}, nil

	case "item.started", "item.completed":
		return parseCodexItemLineTyped(raw.Type, raw.Item, rawCopy)

	default:
		return CodexEvent{Type: raw.Type, Raw: rawCopy}, nil
	}
}

func parseCodexItemLineTyped(eventType string, item *codexItem, rawCopy json.RawMessage) (CodexEvent, error) {
	if item == nil {
		return CodexEvent{Type: eventType, Raw: rawCopy}, nil
	}

	switch item.Type {
	case "agent_message":
		return CodexEvent{
			Type: "assistant",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "assistant",
				Text: item.Text,
			},
		}, nil

	case "command_execution":
		if eventType == "item.started" {
			return CodexEvent{
				Type: "tool_use",
				Raw:  rawCopy,
				Message: &ClaudeMessage{
					Role: "assistant",
					ToolUses: []ToolUseBlock{{
						ID:    item.ID,
						Name:  "Bash",
						Input: map[string]any{"command": item.Command},
					}},
				},
			}, nil
		}
		output := item.AggregatedOutput
		exitCode := 0
		if item.ExitCode != nil {
			exitCode = *item.ExitCode
		}
		if output == "" {
			output = fmt.Sprintf("Command exited with code %d.", exitCode)
		}
		return CodexEvent{
			Type: "tool_result",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "user",
				ToolResults: []ToolResultBlock{{
					ToolUseID: item.ID,
					Content:   output,
					IsError:   exitCode != 0,
				}},
			},
		}, nil

	case "file_change", "collab_tool_call", "web_search":
		// Non-Bash Codex tool actions. Count each once on item.started (mirrors
		// command_execution) so the tool-call metric isn't Bash-only; skip the
		// completed half to avoid double-counting.
		if eventType != "item.started" {
			return CodexEvent{Raw: rawCopy}, nil
		}
		return CodexEvent{
			Type: "tool_use",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "assistant",
				ToolUses: []ToolUseBlock{{
					ID:   item.ID,
					Name: codexToolName(item.Type),
				}},
			},
		}, nil

	default:
		return CodexEvent{
			Type: "assistant",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "assistant",
				Text: item.Text,
			},
		}, nil
	}
}

// codexToolName maps a Codex item type to a display name for its tool use.
func codexToolName(itemType string) string {
	switch itemType {
	case "file_change":
		return "Edit"
	case "collab_tool_call":
		return "MCP"
	case "web_search":
		return "WebSearch"
	default:
		return itemType
	}
}

// extractAssistantContent parses the "message" block from an assistant event.
// Text contains only joined text blocks. ToolUses contains structured tool calls.
func extractAssistantContent(msg map[string]any) ClaudeMessage {
	content, ok := msg["content"].([]any)
	if !ok {
		return ClaudeMessage{Role: "assistant"}
	}
	var textParts []string
	var tools []ToolUseBlock

	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok {
				textParts = append(textParts, text)
			}
		case "tool_use":
			tb := ToolUseBlock{
				ID:   strVal(block, "id"),
				Name: strVal(block, "name"),
			}
			if input, ok := block["input"].(map[string]any); ok {
				tb.Input = input
			}
			tools = append(tools, tb)
		}
	}
	return ClaudeMessage{
		Role:     "assistant",
		Text:     strings.Join(textParts, "\n"),
		ToolUses: tools,
	}
}

func extractAssistantContentTyped(msg *claudeMessagePayload) ClaudeMessage {
	if msg == nil || len(msg.Content) == 0 {
		return ClaudeMessage{Role: "assistant"}
	}
	var textParts []string
	var tools []ToolUseBlock
	for i := range msg.Content {
		block := &msg.Content[i]
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_use":
			tb := ToolUseBlock{
				ID:   block.ID,
				Name: block.Name,
			}
			if len(block.Input) > 0 && !bytes.Equal(block.Input, []byte("null")) {
				var input map[string]any
				if err := json.Unmarshal(block.Input, &input); err == nil {
					tb.Input = input
				}
			}
			tools = append(tools, tb)
		}
	}
	return ClaudeMessage{
		Role:     "assistant",
		Text:     strings.Join(textParts, "\n"),
		ToolUses: tools,
	}
}

// extractToolResults parses tool_result blocks from a "user" message.
// Returns full content without truncation; callers truncate per their needs.
func extractToolResults(msg map[string]any) []ToolResultBlock {
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var results []ToolResultBlock
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] != "tool_result" {
			continue
		}
		tr := ToolResultBlock{
			ToolUseID: strVal(block, "tool_use_id"),
		}
		if isErr, ok := block["is_error"].(bool); ok {
			tr.IsError = isErr
		}
		// Content can be a string or an array of text blocks.
		switch v := block["content"].(type) {
		case string:
			tr.Content = v
		case []any:
			var parts []string
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			tr.Content = strings.Join(parts, "\n")
		}
		results = append(results, tr)
	}
	return results
}

func extractToolResultsTyped(msg *claudeMessagePayload) []ToolResultBlock {
	if msg == nil || len(msg.Content) == 0 {
		return nil
	}
	var results []ToolResultBlock
	for i := range msg.Content {
		block := &msg.Content[i]
		if block.Type != "tool_result" {
			continue
		}
		tr := ToolResultBlock{
			ToolUseID: block.ToolUseID,
			IsError:   block.IsError,
		}
		switch {
		case len(block.Content) == 0 || bytes.Equal(block.Content, []byte("null")):
		case len(block.Content) > 0 && block.Content[0] == '"':
			_ = json.Unmarshal(block.Content, &tr.Content)
		case len(block.Content) > 0 && block.Content[0] == '[':
			var parts []claudeTextBlock
			if err := json.Unmarshal(block.Content, &parts); err == nil {
				text := make([]string, 0, len(parts))
				for _, part := range parts {
					if part.Text != "" {
						text = append(text, part.Text)
					}
				}
				tr.Content = strings.Join(text, "\n")
			}
		}
		results = append(results, tr)
	}
	return results
}

func extractResultFieldsTyped(raw claudeEnvelope) ClaudeResult {
	r := ClaudeResult{
		Subtype:      raw.Subtype,
		Text:         raw.Result,
		SessionID:    raw.SessionID,
		CostUSD:      sanitizeCostUSD(raw.TotalCostUSD),
		InputTokens:  raw.TotalInputTokens,
		OutputTokens: raw.TotalOutputTokens,
	}
	if raw.Usage != nil {
		// usage.input_tokens / usage.output_tokens are the canonical Claude
		// Code fields; total_*_tokens at root only exists in legacy fixtures.
		// Prefer the nested values when present, fall back to root otherwise.
		if raw.Usage.InputTokens != 0 {
			r.InputTokens = raw.Usage.InputTokens
		}
		if raw.Usage.OutputTokens != 0 {
			r.OutputTokens = raw.Usage.OutputTokens
		}
		r.CacheCreationInputTokens = raw.Usage.CacheCreationInputTokens
		r.CacheReadInputTokens = raw.Usage.CacheReadInputTokens
	}
	if raw.IsError || raw.APIStatus != 0 || raw.Error != "" {
		r.ErrorType = raw.Error
		if r.ErrorType == "" && raw.IsError {
			r.ErrorType = "provider_error"
		}
		r.ErrorStatus = raw.APIStatus
	}
	return r
}

func sanitizeCostUSD(cost float64) float64 {
	if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return cost
}

// copyRaw returns an independent copy of line as json.RawMessage.
// scanner.Bytes() aliases the scanner's internal buffer which is overwritten on
// the next Scan() call. Always copy before storing.
func copyRaw(line []byte) json.RawMessage {
	cp := make([]byte, len(line))
	copy(cp, line)
	return cp
}

// strVal extracts a string from a map[string]any, returning "" on any failure.
func strVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// autoModeDenialMarker is the substring Claude Code embeds in tool_result error
// content when the auto-mode classifier blocks a tool call. Matched
// case-insensitively so a minor wording change doesn't silently break detection.
const autoModeDenialMarker = "denied by the claude code auto mode classifier"

// classifyAutoModeDenial reports whether tr is an auto-mode classifier denial.
// True when tr.IsError is set and tr.Content (case-insensitive) contains the
// denial marker, regardless of how Content was assembled (plain string or
// array-joined text blocks).
func classifyAutoModeDenial(tr ToolResultBlock) bool {
	return tr.IsError && strings.Contains(strings.ToLower(tr.Content), autoModeDenialMarker)
}
