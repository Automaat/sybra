package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/dbimport"
)

// IncidentImportDomain names this domain in the import marker table.
const IncidentImportDomain = "incidents"

// ImportIncidents copies the incident files into the database once.
//
// Bounded — an incident is closed once its cause is fixed — so one transaction.
// The files are only read.
func ImportIncidents(ctx context.Context, database *db.DB, dir, scope string, logger *slog.Logger) error {
	return dbimport.Once(ctx, database, IncidentImportDomain, scope, logger, func(ctx context.Context, tx *sql.Tx) (int, error) {
		incidents, err := newIncidentFiles(dir).List()
		if err != nil {
			return 0, err
		}
		for i := range incidents {
			in := incidents[i]
			doc, err := yaml.Marshal(in)
			if err != nil {
				return 0, fmt.Errorf("incident store: encode: %w", err)
			}
			if _, err := tx.ExecContext(ctx, database.Rebind(upsertIncident),
				in.Fingerprint, string(in.State), in.FailureCode, db.TimeValue(in.LastSeen), string(doc)); err != nil {
				return 0, fmt.Errorf("insert incident: %w", err)
			}
		}
		return len(incidents), nil
	})
}
