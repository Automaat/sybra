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

func TestFactoryCLIParsesFractionalRFC3339(t *testing.T) {
	for _, raw := range []string{"2026-09-01T00:00:00.123Z", "2026-09-01T00:00:00.123456789+02:00"} {
		want, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, duration := range []bool{false, true} {
			got, err := factoryTime(raw, time.Now(), duration)
			if err != nil || !got.Equal(want) || got.Nanosecond() != want.Nanosecond() {
				t.Errorf("factoryTime(%q, duration=%v) = %v, %v; want %v", raw, duration, got, err, want)
			}
		}
	}
}
