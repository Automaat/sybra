//go:build e2e

package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/agent"
)

// lastAssistantText returns the content of the last assistant-typed stream
// event. Mirrors the unexported helper of the same name in
// internal/sybra/completion — duplicated here rather than exported since
// this e2e test is the only caller outside that package.
func lastAssistantText(ag *agent.Agent) string {
	out := ag.Output()
	for i := range slices.Backward(out) {
		if out[i].Type == "assistant" {
			return out[i].Content
		}
	}
	return ""
}
