package sybra

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestFactoryUnreadableServerAuditIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	logger, err := audit.NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	if err := os.WriteFile(filepath.Join(dir, "2026-09-01.ndjson"), []byte("synthetic-private-corrupt-record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := AuditService{audit: logger}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err = svc.GetFactoryReport(audit.FactoryQuery{Since: start, Until: start.Add(time.Hour)})
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status == nil || status.HTTPStatus() != 503 {
		t.Fatalf("server-side corruption must be unavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("corrupt record leaked into API error: %v", err)
	}
}
