package monitor

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

// fakeSubmitter is a minimal issueSubmitter for outbox tests: it fails with
// a configured error until markHealthy switches it to always-succeed, so
// tests can simulate "credentials recovered" without a real gh binary.
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   int
	err     error
	healthy bool
}

func (f *fakeSubmitter) SubmitIssue(_ context.Context, title, _ string, _ []string) (created bool, url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.healthy {
		return true, "https://github.com/example/repo/issues/1", nil
	}
	return false, "", f.err
}

func (f *fakeSubmitter) markHealthy() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy = true
}

func (f *fakeSubmitter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestDurableSink(t *testing.T, inner issueSubmitter) (sink *DurableGHIssueSink, outboxDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	store, err := newIssueOutboxStore(filepath.Join(tmpDir, "outbox"))
	if err != nil {
		t.Fatalf("newIssueOutboxStore: %v", err)
	}
	return &DurableGHIssueSink{inner: inner, store: store, logger: slog.Default(), name: "test"}, store.dir
}

var errAuthFailed = errors.New("gh issue create: gh: To get started with GitHub CLI, please run:  gh auth login: exit status 1")

func TestDurableGHIssueSink_AuthFailurePersistsAndLaterRetrySucceeds(t *testing.T) {
	inner := &fakeSubmitter{err: errAuthFailed}
	d, dir := newTestDurableSink(t, inner)

	_, _, err := d.SubmitIssue(context.Background(), "some anomaly", "body", nil)
	if err == nil {
		t.Fatal("expected auth error to propagate to the caller")
	}
	if d.store.depth() != 1 {
		t.Fatalf("want 1 pending outbox item after auth failure, got %d (dir=%s)", d.store.depth(), dir)
	}

	// Credentials "recover": the next call must flush the pending item
	// before (or as part of) attempting its own submission.
	inner.markHealthy()
	_, _, err = d.SubmitIssue(context.Background(), "a different anomaly", "body2", nil)
	if err != nil {
		t.Fatalf("submit after recovery: %v", err)
	}
	if d.store.depth() != 0 {
		t.Fatalf("want 0 pending outbox items after recovery flush, got %d", d.store.depth())
	}
}

func TestDurableGHIssueSink_AuthFailureEmitsAuditEventOnce(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })

	store, err := newIssueOutboxStore(filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatalf("newIssueOutboxStore: %v", err)
	}
	inner := &fakeSubmitter{err: errAuthFailed}
	d := &DurableGHIssueSink{inner: inner, store: store, logger: slog.Default(), name: "monitor", auditLog: auditLog}

	// Same fingerprint fails twice (e.g. one initial failure plus one
	// retried-and-still-failing flush) — the audit event must only be
	// emitted once, on the first time the fingerprint is newly persisted.
	if _, _, err := d.SubmitIssue(context.Background(), "recurring anomaly", "body", nil); err == nil {
		t.Fatal("expected auth failure")
	}
	d.flushPending(context.Background())

	events, err := audit.Read(auditDir, audit.Query{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	var matches int
	for _, e := range events {
		if e.Type != audit.EventGHIssueAuthFailed {
			continue
		}
		matches++
		if sink, _ := e.Data["sink"].(string); sink != "monitor" {
			t.Errorf("event sink = %q, want %q", sink, "monitor")
		}
	}
	if matches != 1 {
		t.Fatalf("want exactly 1 gh_issue.auth_failed audit event, got %d", matches)
	}
}

func TestDurableGHIssueSink_NonAuthErrorNotQueued(t *testing.T) {
	inner := &fakeSubmitter{err: errors.New("gh issue create: label \"bogus\" not found: exit status 1")}
	d, _ := newTestDurableSink(t, inner)

	_, _, err := d.SubmitIssue(context.Background(), "title", "body", nil)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if d.store.depth() != 0 {
		t.Fatalf("non-auth error must not be queued for retry, got depth %d", d.store.depth())
	}
}

func TestDurableGHIssueSink_SurvivesRestartAcrossSinkInstances(t *testing.T) {
	dir := t.TempDir()
	outboxDir := filepath.Join(dir, "outbox")

	failing := &fakeSubmitter{err: errAuthFailed}
	store1, err := newIssueOutboxStore(outboxDir)
	if err != nil {
		t.Fatalf("newIssueOutboxStore: %v", err)
	}
	d1 := &DurableGHIssueSink{inner: failing, store: store1, logger: slog.Default(), name: "test"}
	if _, _, err := d1.SubmitIssue(context.Background(), "persist me", "body", nil); err == nil {
		t.Fatal("expected auth failure")
	}
	if store1.depth() != 1 {
		t.Fatalf("want 1 pending item before restart, got %d", store1.depth())
	}

	// Simulate a Sybra restart: a brand new DurableGHIssueSink pointed at the
	// same on-disk directory, wrapping a now-healthy inner sink.
	healthy := &fakeSubmitter{healthy: true}
	store2, err := newIssueOutboxStore(outboxDir)
	if err != nil {
		t.Fatalf("newIssueOutboxStore (restart): %v", err)
	}
	d2 := &DurableGHIssueSink{inner: healthy, store: store2, logger: slog.Default(), name: "test"}
	d2.flushPending(context.Background())

	if store2.depth() != 0 {
		t.Fatalf("want the pre-restart pending item drained after restart, got depth %d", store2.depth())
	}
	if healthy.callCount() != 1 {
		t.Fatalf("want the restarted sink to have retried the persisted item once, got %d calls", healthy.callCount())
	}
}

func TestDurableGHIssueSink_DropsAfterMaxAttempts(t *testing.T) {
	origMax := issueOutboxMaxAttempts
	issueOutboxMaxAttempts = 3
	t.Cleanup(func() { issueOutboxMaxAttempts = origMax })

	inner := &fakeSubmitter{err: errAuthFailed}
	d, _ := newTestDurableSink(t, inner)

	if _, _, err := d.SubmitIssue(context.Background(), "stuck forever", "body", nil); err == nil {
		t.Fatal("expected auth failure")
	}
	if d.store.depth() != 1 {
		t.Fatalf("want 1 pending item, got %d", d.store.depth())
	}

	// The initial SubmitIssue failure above persists the item at Attempts=0
	// (a fresh, not-yet-retried entry). Each flush that still fails bumps
	// Attempts by one; three failing flushes exhausts the 3-attempt budget
	// and the item is dropped on the third.
	d.flushPending(context.Background())
	d.flushPending(context.Background())
	d.flushPending(context.Background())

	if d.store.depth() != 0 {
		t.Fatalf("want the item dropped once max attempts is exceeded, got depth %d", d.store.depth())
	}
}

func TestDurableGHIssueSink_BoundedDepth(t *testing.T) {
	origMax := issueOutboxMaxDepth
	issueOutboxMaxDepth = 2
	t.Cleanup(func() { issueOutboxMaxDepth = origMax })

	inner := &fakeSubmitter{err: errAuthFailed}
	d, _ := newTestDurableSink(t, inner)

	titles := []string{"one", "two", "three"}
	for _, title := range titles {
		if _, _, err := d.SubmitIssue(context.Background(), title, "body", nil); err == nil {
			t.Fatalf("expected auth failure for %q", title)
		}
	}

	if d.store.depth() > issueOutboxMaxDepth {
		t.Fatalf("outbox depth %d exceeded bound %d", d.store.depth(), issueOutboxMaxDepth)
	}
}

func TestDurableGHIssueSink_SubmitDelegatesToAnomalyShapedSubmitIssue(t *testing.T) {
	inner := &fakeSubmitter{healthy: true}
	d, _ := newTestDurableSink(t, inner)

	a := Anomaly{Kind: KindOverDispatchLimit, Fingerprint: "fp"}
	created, err := d.Submit(context.Background(), a, "body")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatal("expected created=true from the healthy fake")
	}
}

func TestRedactSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "classic personal access token",
			in:   "gh: HTTP 401: Bad credentials (token ghp_ABCDEFGHIJ0123456789KLMN used)",
			want: "gh: HTTP 401: Bad credentials (token [redacted] used)",
		},
		{
			name: "installation token prefix",
			in:   "curl -H 'Authorization: Bearer ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345'",
			want: "curl -H 'Authorization: Bearer [redacted]'",
		},
		{
			name: "no token present",
			in:   "gh: HTTP 500: internal error",
			want: "gh: HTTP 500: internal error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(redactSecrets([]byte(tt.in))); got != tt.want {
				t.Errorf("redactSecrets(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
