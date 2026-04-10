package agent

import (
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

// ParseClaudeLine parses one line of Claude stream-json output.
// The returned ClaudeEvent.Raw is an independent copy safe to keep after the
// scanner buffer is reused.
func ParseClaudeLine(line []byte) (ClaudeEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return ClaudeEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	// ok intentionally discarded: missing/non-string type yields "" which is
	// handled below by the default case.
	eventType, _ := raw["type"].(string)
	event := ClaudeEvent{
		Type:    eventType,
		Subtype: strVal(raw, "subtype"),
		Raw:     copyRaw(line),
	}

	switch eventType {
	case "system":
		// ok intentionally discarded: zero-value "" is safe when session_id absent.
		event.SessionID, _ = raw["session_id"].(string)

	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		if msg != nil {
			m := extractAssistantContent(msg)
			event.Message = &m
		}
		event.SessionID = strVal(raw, "session_id")

	case "user":
		msg, _ := raw["message"].(map[string]any)
		if msg != nil {
			results := extractToolResults(msg)
			event.Message = &ClaudeMessage{Role: "user", ToolResults: results}
		}

	case "result":
		r := extractResultFields(raw)
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
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return CodexEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	rawCopy := copyRaw(line)
	eventType := strVal(raw, "type")

	switch eventType {
	case "thread.started":
		return CodexEvent{Type: "init", SessionID: strVal(raw, "thread_id"), Raw: rawCopy}, nil

	case "turn.started":
		return CodexEvent{Type: "init", Raw: rawCopy}, nil

	case "error":
		return CodexEvent{
			Type:    "result",
			Subtype: "error",
			Raw:     rawCopy,
			Result:  &ClaudeResult{Subtype: "error", Text: strVal(raw, "message")},
		}, nil

	case "turn.completed":
		usage, _ := raw["usage"].(map[string]any)
		var r ClaudeResult
		if usage != nil {
			r.InputTokens = int(floatVal(usage, "input_tokens"))
			r.OutputTokens = int(floatVal(usage, "output_tokens"))
		}
		return CodexEvent{Type: "result", Raw: rawCopy, Result: &r}, nil

	case "item.started", "item.completed":
		return parseCodexItemLine(eventType, raw, rawCopy)

	default:
		return CodexEvent{Type: eventType, Raw: rawCopy}, nil
	}
}

func parseCodexItemLine(eventType string, raw map[string]any, rawCopy json.RawMessage) (CodexEvent, error) {
	item, _ := raw["item"].(map[string]any)
	itemType := strVal(item, "type")

	switch itemType {
	case "agent_message":
		return CodexEvent{
			Type: "assistant",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "assistant",
				Text: strVal(item, "text"),
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
						ID:    strVal(item, "id"),
						Name:  "Bash",
						Input: map[string]any{"command": strVal(item, "command")},
					}},
				},
			}, nil
		}
		output := strVal(item, "aggregated_output")
		exitCode := int(floatVal(item, "exit_code"))
		if output == "" {
			output = fmt.Sprintf("Command exited with code %d.", exitCode)
		}
		return CodexEvent{
			Type: "tool_result",
			Raw:  rawCopy,
			Message: &ClaudeMessage{
				Role: "user",
				ToolResults: []ToolResultBlock{{
					ToolUseID: strVal(item, "id"),
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
				Text: strVal(item, "text"),
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

// extractResultFields parses cost, token, and session fields from a "result" event.
func extractResultFields(raw map[string]any) ClaudeResult {
	r := ClaudeResult{
		Subtype:   strVal(raw, "subtype"),
		SessionID: strVal(raw, "session_id"),
	}
	// ok intentionally discarded: zero-value "" is acceptable when result absent.
	r.Text, _ = raw["result"].(string)
	if cost, ok := raw["total_cost_usd"].(float64); ok {
		r.CostUSD = cost
	}
	if v, ok := raw["total_input_tokens"].(float64); ok {
		r.InputTokens = int(v)
	}
	if v, ok := raw["total_output_tokens"].(float64); ok {
		r.OutputTokens = int(v)
	}
	return r
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

// floatVal extracts a float64 from a map[string]any, returning 0 on any failure.
func floatVal(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, _ := m[key].(float64)
	return v
}
