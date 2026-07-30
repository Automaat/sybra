package evaluation

import (
	"testing"
	"time"
)

func TestTrustworthy_FreshCurrentSchemaIsTrustworthy(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rep := Report{SchemaVersion: ScorecardSchemaVersion, GeneratedAt: now.Add(-time.Hour)}

	got := Trustworthy(rep, now, 6*time.Hour)

	if !got.Trustworthy || got.Reason != "" {
		t.Fatalf("Trustworthy = %+v, want trustworthy with no reason", got)
	}
}

func TestTrustworthy_MismatchedSchemaVersionIsUntrustworthy(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rep := Report{SchemaVersion: ScorecardSchemaVersion - 1, GeneratedAt: now}

	got := Trustworthy(rep, now, 6*time.Hour)

	if got.Trustworthy {
		t.Fatal("Trustworthy = true, want false for a schema-version mismatch")
	}
	if got.Reason == "" {
		t.Fatal("Reason is empty, want an explanation")
	}
}

func TestTrustworthy_StaleReportIsUntrustworthy(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rep := Report{SchemaVersion: ScorecardSchemaVersion, GeneratedAt: now.Add(-48 * time.Hour)}

	got := Trustworthy(rep, now, 6*time.Hour)

	if got.Trustworthy {
		t.Fatal("Trustworthy = true, want false for a report older than maxAge")
	}
}

func TestTrustworthy_ZeroSchemaVersionOrGeneratedAtFailOpen(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// A hand-built fixture (common in existing tests) with no SchemaVersion
	// or GeneratedAt set must not be spuriously rejected as stale/mismatched.
	rep := Report{}

	got := Trustworthy(rep, now, 6*time.Hour)

	if !got.Trustworthy {
		t.Fatalf("Trustworthy = %+v, want trustworthy (fail-open on unset signals)", got)
	}
}

func TestTrustworthy_MaxAgeDisabledSkipsFreshnessCheck(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rep := Report{SchemaVersion: ScorecardSchemaVersion, GeneratedAt: now.Add(-24 * 365 * time.Hour)}

	got := Trustworthy(rep, now, 0)

	if !got.Trustworthy {
		t.Fatalf("Trustworthy = %+v, want trustworthy when maxAge <= 0 disables the check", got)
	}
}
