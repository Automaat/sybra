package agent

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
)

// toolSignature returns a canonical fingerprint of a set of tool calls (each
// tool's name plus its JSON-canonicalized input), order-independent, or "" when
// there are no tool calls. The watchdog's loop detector compares consecutive
// signatures to spot an agent repeating the same call. json.Marshal sorts map
// keys, so the same call always hashes identically.
func toolSignature(toolUses []ToolUseBlock) string {
	if len(toolUses) == 0 {
		return ""
	}
	parts := make([]string, 0, len(toolUses))
	for i := range toolUses {
		var b strings.Builder
		b.WriteString(toolUses[i].Name)
		if toolUses[i].Input != nil {
			if raw, err := json.Marshal(toolUses[i].Input); err == nil {
				b.Write(raw)
			}
		}
		parts = append(parts, b.String())
	}
	slices.Sort(parts)
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// claudeEventToStreamEvent converts a shared ClaudeEvent into a StreamEvent
// for the headless runner. Tool uses are formatted as "[name] cmd/desc" strings.
// Tool results are truncated to 500 chars.
func claudeEventToStreamEvent(e ClaudeEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "system", "init":
		ev.PluginErrors = e.PluginErrors
	case "assistant":
		if e.Message != nil {
			ev.Content = formatHeadlessAssistant(e.Message)
			ev.PlanSteps = extractTodoWriteSteps(e.Message.ToolUses)
			ev.ToolCalls = len(e.Message.ToolUses)
			ev.toolSig = toolSignature(e.Message.ToolUses)
		}
	case "user":
		if e.Message != nil {
			ev.Content = formatHeadlessToolResults(e.Message.ToolResults)
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
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

// extractTodoWriteSteps scans tool uses for a TodoWrite call and returns the
// parsed todo list. Returns nil if no TodoWrite call is present or parsing fails.
func extractTodoWriteSteps(toolUses []ToolUseBlock) []PlanStep {
	for i := range toolUses {
		if toolUses[i].Name != "TodoWrite" {
			continue
		}
		todosRaw, ok := toolUses[i].Input["todos"]
		if !ok {
			return nil
		}
		items, ok := todosRaw.([]any)
		if !ok {
			return nil
		}
		steps := make([]PlanStep, 0, len(items))
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, _ := m["content"].(string)
			status, _ := m["status"].(string)
			if content == "" {
				continue
			}
			steps = append(steps, PlanStep{Content: content, Status: status})
		}
		return steps
	}
	return nil
}

// codexEventToStreamEvent converts a shared CodexEvent into a StreamEvent
// for the headless runner.
func codexEventToStreamEvent(e CodexEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Content = e.Message.Text
		}
	case "tool_use":
		// Codex tool item types (command_execution, file_change, collab_tool_call,
		// web_search) are mapped to tool_use in parseCodexItemLineTyped and counted
		// here once per item.started.
		if e.Message != nil && len(e.Message.ToolUses) > 0 {
			cmd, _ := e.Message.ToolUses[0].Input["command"].(string)
			ev.Content = cmd
			ev.ToolCalls = len(e.Message.ToolUses)
			ev.toolSig = toolSignature(e.Message.ToolUses)
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.CostUSD = e.Result.CostUSD
			ev.InputTokens = e.Result.InputTokens
			ev.OutputTokens = e.Result.OutputTokens
			ev.CacheCreationInputTokens = e.Result.CacheCreationInputTokens
			ev.CacheReadInputTokens = e.Result.CacheReadInputTokens
			ev.ReasoningTokens = e.Result.ReasoningTokens
			ev.ErrorType = e.Result.ErrorType
			ev.ErrorStatus = e.Result.ErrorStatus
		}
	}
	return ev
}

// copilotEventToStreamEvent converts a parsed CopilotEvent into a StreamEvent
// for the headless runner. Mirrors codexEventToStreamEvent: assistant text,
// tool_use command, tool_result content. Copilot reports output tokens
// per-message (carried on assistant events) and premium-request usage on the
// result event; it provides no USD cost or input/cache token breakdown.
func copilotEventToStreamEvent(e CopilotEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Content = e.Message.Text
		}
		ev.OutputTokens = e.OutputTokens
	case "tool_use":
		if e.Message != nil && len(e.Message.ToolUses) > 0 {
			cmd, _ := e.Message.ToolUses[0].Input["command"].(string)
			ev.Content = cmd
			ev.ToolCalls = len(e.Message.ToolUses)
			ev.toolSig = toolSignature(e.Message.ToolUses)
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.PremiumRequests = e.Result.PremiumRequests
			ev.ErrorType = e.Result.ErrorType
			ev.ErrorStatus = e.Result.ErrorStatus
		}
	}
	return ev
}

// formatHeadlessAssistant produces the flat content string for headless assistant
// events: joined text parts followed by "[name] cmd/desc" tool use lines.
func formatHeadlessAssistant(msg *ClaudeMessage) string {
	var parts []string
	if msg.Text != "" {
		parts = append(parts, msg.Text)
	}
	for _, tu := range msg.ToolUses {
		if tu.Input == nil {
			parts = append(parts, fmt.Sprintf("[%s]", tu.Name))
			continue
		}
		desc, _ := tu.Input["description"].(string)
		cmd, _ := tu.Input["command"].(string)
		switch {
		case desc != "":
			parts = append(parts, fmt.Sprintf("[%s] %s", tu.Name, desc))
		case cmd != "":
			parts = append(parts, fmt.Sprintf("[%s] %s", tu.Name, cmd))
		default:
			parts = append(parts, fmt.Sprintf("[%s]", tu.Name))
		}
	}
	return strings.Join(parts, "\n")
}

// formatHeadlessToolResults joins tool result contents, truncating each to 500 chars.
func formatHeadlessToolResults(results []ToolResultBlock) string {
	var parts []string
	for _, tr := range results {
		content := tr.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		if content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n")
}
