package sybra

import (
	"errors"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

// AuditService reads the instance's own audit log.
//
// sybra-cli cannot read it over the wire any other way: the log is a directory
// of daily files under the server's home, so a client reading its own copy
// would answer questions about the wrong machine. `audit` and `stats
// lifecycle` both reduce the same event stream, so both go through this one
// query. Factory reports additionally reduce on the server so their response
// is bounded and contains no task/project content.
type AuditService struct {
	audit audit.Store
}

// QueryAuditEvents returns the events in the window that match the filters.
func (s *AuditService) QueryAuditEvents(q audit.Query) ([]audit.Event, error) {
	if s.audit == nil {
		return nil, unavailableError("audit log unavailable")
	}
	events, err := s.audit.Read(q)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// GetFactoryReport returns bounded, metadata-only aggregates from this board.
func (s *AuditService) GetFactoryReport(q audit.FactoryQuery) (audit.FactoryReport, error) {
	if err := q.Validate(); err != nil {
		return audit.FactoryReport{}, validationError(err.Error())
	}
	events, err := s.QueryAuditEvents(audit.Query{Since: q.Since, Until: q.Until.Add(-time.Nanosecond), Limit: audit.FactoryMaxEvents + 1, Strict: true, MaxBytes: audit.FactoryMaxBytes})
	if err != nil {
		if errors.Is(err, audit.ErrReadBudget) {
			return audit.FactoryReport{}, validationError(err.Error())
		}
		return audit.FactoryReport{}, unavailableError("factory audit input unavailable; inspect the audit store")
	}
	report, err := audit.SummarizeFactory(events, q)
	if err != nil {
		return audit.FactoryReport{}, validationError(err.Error())
	}
	return report, nil
}
