package main

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestFactoryCLIUsesAggregateEndpoint(t *testing.T) {
	rec := newRecordingServer(t, audit.FactoryReport{})
	if code := cmdFactory(rec.client(), []string{"--since", "2026-09-01", "--until", "2026-09-02"}, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/api/AuditService/GetFactoryReport" {
		t.Fatalf("paths: %v", rec.paths)
	}
}

func TestFactoryCLIRejectsInvalidBoundaries(t *testing.T) {
	for _, raw := range []string{"", "garbage", "-1h", "9999999999999999999d", "32d"} {
		if _, err := factoryTime(raw, time.Now(), true); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
