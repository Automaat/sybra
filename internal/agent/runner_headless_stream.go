package agent

import (
	"fmt"
	"strings"
)

// claudeEventToStreamEvent converts a shared ClaudeEvent into a StreamEvent
// for the headless runner. Tool uses are formatted as "[name] cmd/desc"
// strings. Tool results stay raw here; processHeadlessLine applies the shared
// bounded/artifact-backed rewrite before Sybra stores the event.
func claudeEventToStreamEvent(e ClaudeEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID, parentToolUseID: e.ParentToolUseID}
	switch e.Type {
	case "system", "init":
		ev.PluginErrors = e.PluginErrors
		if e.Subtype == "background_tasks_changed" {
			ev.BackgroundTaskIDs = e.BackgroundTaskIDs
		}
	case "assistant":
		if e.Message != nil {
			ev.Content = formatHeadlessAssistant(e.Message)
			ev.PlanSteps = extractTodoWriteSteps(e.Message.ToolUses)
			ev.ToolCalls = len(e.Message.ToolUses)
			obs := toolLoopObservationForUses(e.Message.ToolUses)
			ev.toolSig = obs.signature
			ev.toolLoopLabel = obs.label
			ev.toolUses = e.Message.ToolUses
		}
	case "user":
		if e.Message != nil {
			ev.Content = formatHeadlessToolResults(e.Message.ToolResults)
			ev.toolResults = e.Message.ToolResults
			for _, tr := range e.Message.ToolResults {
				if classifyAutoModeDenial(tr) {
					ev.permissionDenials = append(ev.permissionDenials, PermissionDenial{
						ToolUseID: tr.ToolUseID,
						Reason:    tr.Content,
					})
				}
			}
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
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID, LimitSnapshot: e.Limits}
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
			obs := toolLoopObservationForUses(e.Message.ToolUses)
			ev.toolSig = obs.signature
			ev.toolLoopLabel = obs.label
			ev.toolUses = e.Message.ToolUses
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
			ev.toolResults = e.Message.ToolResults
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
			obs := toolLoopObservationForUses(e.Message.ToolUses)
			ev.toolSig = obs.signature
			ev.toolLoopLabel = obs.label
			ev.toolUses = e.Message.ToolUses
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
			ev.toolResults = e.Message.ToolResults
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

// opencodeEventToStreamEvent converts OpenCode JSON output into the same
// lightweight event shape used by the headless runner.
func opencodeEventToStreamEvent(e OpenCodeEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Content = e.Message.Text
			ev.ToolCalls = len(e.Message.ToolUses)
			obs := toolLoopObservationForUses(e.Message.ToolUses)
			ev.toolSig = obs.signature
			ev.toolLoopLabel = obs.label
			ev.toolUses = e.Message.ToolUses
		}
		ev.OutputTokens = e.OutputTokens
	case "tool_use":
		if e.Message != nil && len(e.Message.ToolUses) > 0 {
			cmd, _ := e.Message.ToolUses[0].Input["command"].(string)
			if cmd == "" {
				cmd, _ = e.Message.ToolUses[0].Input["input"].(string)
			}
			ev.Content = cmd
			ev.ToolCalls = len(e.Message.ToolUses)
			obs := toolLoopObservationForUses(e.Message.ToolUses)
			ev.toolSig = obs.signature
			ev.toolLoopLabel = obs.label
			ev.toolUses = e.Message.ToolUses
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
			ev.toolResults = e.Message.ToolResults
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.CostUSD = e.Result.CostUSD
			ev.InputTokens = e.Result.InputTokens
			ev.OutputTokens = e.Result.OutputTokens
			ev.CacheReadInputTokens = e.Result.CacheReadInputTokens
			ev.ReasoningTokens = e.Result.ReasoningTokens
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

// formatHeadlessToolResults joins tool result contents without truncation.
// processHeadlessLine applies the bounded/artifact-backed rewrite later.
func formatHeadlessToolResults(results []ToolResultBlock) string {
	var parts []string
	for _, tr := range results {
		if tr.Content != "" {
			parts = append(parts, tr.Content)
		}
	}
	return strings.Join(parts, "\n")
}
