package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ClaudeMessage holds parsed content blocks from assistant or user messages.
type ClaudeMessage struct {
	Role        string            // "assistant" or "user"
	Text        string            // joined text blocks only
	ToolUses    []ToolUseBlock    // tool_use blocks (structured)
	ToolResults []ToolResultBlock // tool_result blocks (structured, untruncated)
}

// ClaudeResult holds fields from "result" events.
type ClaudeResult struct {
	Subtype      string
	Text         string
	SessionID    string
	CostUSD      float64
	InputTokens  int
	OutputTokens int
	// ErrorType and ErrorStatus carry structured error info when Subtype == "error".
	// Codex: mapped from the "code" field. Claude: reserved for future extraction.
	ErrorType   string
	ErrorStatus int
}

// ClaudeEvent is the shared envelope for all Claude stream-json events.
type ClaudeEvent struct {
	Type      string
	Subtype   string
	SessionID string
	Raw       json.RawMessage // independent copy, never aliased to scanner buffer
	Message   *ClaudeMessage
	Result    *ClaudeResult
}

// CodexEvent is the shared envelope for all Codex stream-json events.
type CodexEvent struct {
	Type      string
	Subtype   string
	SessionID string
	Raw       json.RawMessage
	Message   *ClaudeMessage // reuses ClaudeMessage for shared content structure
	Result    *ClaudeResult
}

type claudeEnvelope struct {
	Type              string                `json:"type"`
	Subtype           string                `json:"subtype"`
	SessionID         string                `json:"session_id"`
	Message           *claudeMessagePayload `json:"message"`
	Result            string                `json:"result"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	TotalInputTokens  int                   `json:"total_input_tokens"`
	TotalOutputTokens int                   `json:"total_output_tokens"`
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
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
		Type:    raw.Type,
		Subtype: raw.Subtype,
		Raw:     copyRaw(line),
	}

	switch raw.Type {
	case "system":
		event.SessionID = raw.SessionID

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
	return ClaudeResult{
		Subtype:      raw.Subtype,
		Text:         raw.Result,
		SessionID:    raw.SessionID,
		CostUSD:      raw.TotalCostUSD,
		InputTokens:  raw.TotalInputTokens,
		OutputTokens: raw.TotalOutputTokens,
	}
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
