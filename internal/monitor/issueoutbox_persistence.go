package monitor

import "log/slog"

// IssueOutboxPersistence holds pending GitHub issue filings. Two
// implementations exist: the per-fingerprint YAML files and the database-backed
// SQLIssueOutbox, selected by config.Database.Backend.
//
// Losing an entry here files an issue twice or never: the outbox is what makes
// a filing survive a crash between deciding to file and GitHub accepting it.
type IssueOutboxPersistence interface {
	put(it outboxItem) error
	del(fingerprint string) error
	load(log *slog.Logger) []outboxItem
	// get returns one pending filing. The sink reached around the store to
	// read the file directly, which a database backend has none of.
	get(fingerprint string) (outboxItem, bool)
	// depth reports how many filings are pending, for the bound the sink puts
	// on the outbox and for the operator readout.
	depth() int
}

var _ IssueOutboxPersistence = (*issueOutboxStore)(nil)
