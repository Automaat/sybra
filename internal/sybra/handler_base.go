package sybra

import (
	"log/slog"

	"github.com/Automaat/sybra/internal/audit"
)

// DomainHandler is an embeddable base for handlers that need structured audit
// logging. Embed by value; zero-value audit field silently no-ops.
type DomainHandler struct {
	logger *slog.Logger
	audit  *audit.Logger
	emit   func(string, any)
}

func (h *DomainHandler) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		h.logger.Error("audit.log", "type", eventType, "err", err)
	}
}
