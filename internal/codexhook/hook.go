// Package codexhook maps the JSON payload that codex pipes on stdin to a hook
// process into a structural-only audit.Event. It is the single place that
// defines what data we keep from hook payloads — only session/subagent
// identifiers and the model name. Sensitive fields (cwd, transcript_path,
// tool_input, prompts, file paths, command strings) are never extracted.
package codexhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

// Payload is the subset of fields Sybra reads from the JSON object codex pipes
// on stdin when a hook fires. Unknown fields are silently ignored.
type Payload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	SubagentID    string `json:"subagent_id"`
	Kind          string `json:"kind"`
	// Deliberately excluded: cwd, transcript_path, tool_input, prompts, file
	// paths, command strings, snippets — structural-only fields only.
}

// knownEvents maps the codex hook event name to an audit event type constant.
var knownEvents = map[string]string{
	"SessionStart":  audit.EventCodexSessionStart,
	"SubagentStart": audit.EventCodexSubagentStart,
	"SubagentStop":  audit.EventCodexSubagentStop,
	"Stop":          audit.EventCodexSessionStop,
}

// Map parses raw JSON hook payload bytes and maps them to an audit.Event for
// the given taskID. Returns an error for unknown event names or malformed JSON
// — callers should treat errors as fail-open (exit 0, log to stderr only).
//
// The returned Data map carries only structural fields: hook_event_name,
// session_id, model, subagent_id, kind. The marshaled event line is always
// well under 4096 bytes with these constraints.
func Map(raw []byte, taskID string) (audit.Event, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return audit.Event{}, fmt.Errorf("unmarshal codex hook payload: %w", err)
	}
	eventType, ok := knownEvents[p.HookEventName]
	if !ok {
		return audit.Event{}, fmt.Errorf("unknown codex hook event %q", p.HookEventName)
	}

	data := map[string]any{
		"hook_event_name": p.HookEventName,
	}
	if p.SessionID != "" {
		data["session_id"] = p.SessionID
	}
	if p.Model != "" {
		data["model"] = p.Model
	}
	if p.SubagentID != "" {
		data["subagent_id"] = p.SubagentID
	}
	if p.Kind != "" {
		data["kind"] = p.Kind
	}

	return audit.Event{
		Timestamp: time.Now().UTC(),
		Type:      eventType,
		TaskID:    taskID,
		Data:      data,
	}, nil
}
