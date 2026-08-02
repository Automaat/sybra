package audit

import (
	"path/filepath"
	"testing"
	"time"
)

// TestReadFindsEventsAcrossZoneDateBoundary pins the writer/reader agreement
// that #2826 broke. Logger names each file from the event's UTC timestamp,
// while callers build their window from time.Now(), which carries the local
// zone. For the hours where the local and UTC dates differ, formatting the
// window in local time makes the reader look for a filename the writer never
// produced — and the query returns zero events rather than an error, so the
// failure is silent.
func TestReadFindsEventsAcrossZoneDateBoundary(t *testing.T) {
	t.Parallel()

	// 2026-08-01T22:50Z is already 2026-08-02 in CEST (+02:00) and still
	// 2026-08-01 in UTC — exactly the window that used to read empty.
	eventUTC := time.Date(2026, 8, 1, 22, 50, 0, 0, time.UTC)

	cases := []struct {
		name string
		zone *time.Location
	}{
		{name: "ahead of utc, local date already rolled", zone: time.FixedZone("CEST", 2*60*60)},
		{name: "behind utc, local date not yet rolled", zone: time.FixedZone("PST", -8*60*60)},
		{name: "utc itself", zone: time.UTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			l, err := NewLogger(dir)
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			if err := l.Log(Event{Type: EventRoutingReweighted, Timestamp: eventUTC}); err != nil {
				t.Fatalf("Log: %v", err)
			}

			// The window a caller would build with time.Now() in that zone.
			local := eventUTC.In(tc.zone)
			got, err := Read(dir, Query{
				Since: local.Add(-time.Minute),
				Until: local.Add(time.Minute),
				Type:  EventRoutingReweighted,
			})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("Read returned %d events, want 1 — the window was built in %s, where the local date is %s but the file is named %s",
					len(got), tc.zone, local.Format(time.DateOnly), eventUTC.Format(time.DateOnly))
			}
		})
	}
}

// TestAuditFilesSelectsByUTCDate pins the selection directly, so a regression
// is attributed to file naming rather than to event filtering.
func TestAuditFilesSelectsByUTCDate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	eventUTC := time.Date(2026, 8, 1, 23, 30, 0, 0, time.UTC)
	if err := l.Log(Event{Type: "x", Timestamp: eventUTC}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	local := eventUTC.In(time.FixedZone("CEST", 2*60*60))
	paths, err := auditFiles(dir, local.Add(-time.Minute), local.Add(time.Minute))
	if err != nil {
		t.Fatalf("auditFiles: %v", err)
	}
	want := filepath.Join(dir, "2026-08-01.ndjson")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("auditFiles = %v, want [%s]", paths, want)
	}
}
