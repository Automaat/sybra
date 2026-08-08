package sybra

import (
	"github.com/Automaat/sybra/internal/audit"
)

// AuditService reads the instance's own audit log.
//
// sybra-cli cannot read it over the wire any other way: the log is a directory
// of daily files under the server's home, so a client reading its own copy
// would answer questions about the wrong machine. `audit` and `stats
// lifecycle` both reduce the same event stream, so both go through this one
// query rather than a report endpoint each.
type AuditService struct {
	auditDir string
}

// QueryAuditEvents returns the events in the window that match the filters.
func (s *AuditService) QueryAuditEvents(q audit.Query) ([]audit.Event, error) {
	if s.auditDir == "" {
		return nil, unavailableError("audit log unavailable")
	}
	events, err := audit.Read(s.auditDir, q)
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []audit.Event{}
	}
	return events, nil
}
