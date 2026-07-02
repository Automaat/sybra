package learning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/stats"
)

type fakeStats struct{ records []stats.RunRecord }

func (f fakeStats) All() []stats.RunRecord { return f.records }

type fakeAudit struct {
	events []audit.Event
	err    error
}

func (f fakeAudit) Read(audit.Query) ([]audit.Event, error) { return f.events, f.err }

// alwaysBlockGate reports every provider unhealthy, so llmexec.RunJSON's
// candidate loop skips exec.LookPath entirely for every candidate — a
// deterministic "summarizer unavailable" failure with no process spawned.
type alwaysBlockGate struct{}

func (alwaysBlockGate) IsHealthy(string) bool                         { return false }
func (alwaysBlockGate) RateLimited(string) bool                       { return false }
func (alwaysBlockGate) Failover(string) string                        { return "" }
func (alwaysBlockGate) Reason(string) string                          { return "blocked for test" }
func (alwaysBlockGate) ReportAuthFailure(string, string)              {}
func (alwaysBlockGate) ReportRateLimit(string, time.Duration, string) {}

func newTestService(t *testing.T, d Deps) *Service {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(store) returned error: %v", err)
	}
	if d.Store == nil {
		d.Store = store
	}
	if d.Cfg.MinRuns == 0 {
		d.Cfg.MinRuns = 1
	}
	if d.Cfg.MinLandings == 0 {
		d.Cfg.MinLandings = 1
	}
	if d.Cfg.WindowDays == 0 {
		d.Cfg.WindowDays = 7
	}
	if d.Cfg.MaxWindowDays == 0 {
		d.Cfg.MaxWindowDays = 30
	}
	return NewService(d)
}

func TestRunNowRejectsWhenInsufficientFreshData(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	svc := newTestService(t, Deps{
		Cfg:   config.LearningDigestConfig{MinRuns: 5, MinLandings: 5, WindowDays: 7, MaxWindowDays: 30},
		Stats: fakeStats{},
		Audit: fakeAudit{},
		Now:   func() time.Time { return now },
		Gate:  alwaysBlockGate{},
	})

	_, err := svc.RunNow(context.Background())
	if err == nil {
		t.Fatal("RunNow succeeded despite empty stats/audit, want an insufficient-data error")
	}
	if !strings.Contains(err.Error(), "insufficient fresh data") {
		t.Fatalf("err = %v, want an insufficient-fresh-data message", err)
	}
	if _, ok, _ := svc.store.Latest(); ok {
		t.Fatal("a rejected RunNow must not persist any digest")
	}
}

func TestRunNowSummarizerFailureLeavesPreviousDigestIntact(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	in := now.Add(-time.Hour)

	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(store) returned error: %v", err)
	}
	seed := Digest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.AddDate(0, 0, -7),
		Since:         now.AddDate(0, 0, -14),
		Until:         now.AddDate(0, 0, -7),
		ReportDigest:  "seed-report",
		Worked:        []string{"seed finding"},
		NextBets:      []string{"seed bet"},
	}
	if _, err := store.Put(seed); err != nil {
		t.Fatalf("seed Put returned error: %v", err)
	}

	auditLog := &recordingAuditLog{}
	svc := newTestService(t, Deps{
		Cfg:      config.LearningDigestConfig{MinRuns: 1, MinLandings: 1, WindowDays: 7, MaxWindowDays: 30},
		Store:    store,
		Stats:    fakeStats{records: []stats.RunRecord{{TaskID: "A", Timestamp: in}}},
		Audit:    fakeAudit{events: []audit.Event{{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in}}},
		AuditLog: auditLog,
		Now:      func() time.Time { return now },
		Gate:     alwaysBlockGate{},
	})

	_, err = svc.RunNow(context.Background())
	if err == nil {
		t.Fatal("RunNow succeeded despite a fully-blocked provider gate, want a summarizer error")
	}

	latest, ok, lerr := store.Latest()
	if lerr != nil {
		t.Fatalf("Latest returned error: %v", lerr)
	}
	if !ok || latest.ReportDigest != "seed-report" {
		t.Fatalf("Latest = %+v (ok=%v), want the seeded digest left untouched", latest, ok)
	}

	if len(auditLog.events) != 1 || auditLog.events[0].Type != audit.EventLearningDigestFailed {
		t.Fatalf("audit events = %+v, want exactly one EventLearningDigestFailed", auditLog.events)
	}
}

func TestRunNowSingleFlight(t *testing.T) {
	svc := newTestService(t, Deps{
		Cfg:  config.LearningDigestConfig{MinRuns: 1, MinLandings: 1, WindowDays: 7, MaxWindowDays: 30},
		Gate: alwaysBlockGate{},
	})

	svc.genMu.Lock()
	defer svc.genMu.Unlock()

	_, err := svc.RunNow(context.Background())
	if err == nil {
		t.Fatal("RunNow succeeded while a generation was already in progress")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("err = %v, want an already-in-progress message", err)
	}
}

func TestRunNowSuccessPersists(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -7)
	in := since.Add(time.Hour)

	summarizer := func(_ context.Context, prompt string, opts llmexec.Options) (llmexec.Result, error) {
		if opts.Provider != "claude" {
			t.Fatalf("summarizer called with provider %q, want claude", opts.Provider)
		}
		sinceEcho, untilEcho := extractWindowFromPrompt(t, prompt)
		body := `{
  "since": "` + sinceEcho + `",
  "until": "` + untilEcho + `",
  "worked": ["claude sonnet landed cleanly"],
  "notWorked": [],
  "uncertain": [],
  "nextBets": ["try a longer prompt"]
}`
		return llmexec.Result{Provider: "claude", Text: body, CostUSD: 0.05}, nil
	}

	auditLog := &recordingAuditLog{}
	var emitted []string
	svc := newTestService(t, Deps{
		Cfg:        config.LearningDigestConfig{MinRuns: 1, MinLandings: 1, WindowDays: 7, MaxWindowDays: 30, Model: "sonnet"},
		Stats:      fakeStats{records: []stats.RunRecord{{TaskID: "A", Timestamp: in}}},
		Audit:      fakeAudit{events: []audit.Event{{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in}}},
		AuditLog:   auditLog,
		Now:        func() time.Time { return now },
		Summarizer: summarizer,
		Emit:       func(event string, _ any) { emitted = append(emitted, event) },
	})

	d, err := svc.RunNow(context.Background())
	if err != nil {
		t.Fatalf("RunNow returned error: %v", err)
	}
	if len(d.Worked) != 1 || d.AuthorProvider != "claude" {
		t.Fatalf("digest = %+v, want worked populated and provider=claude", d)
	}
	if _, ok, _ := svc.store.Latest(); !ok {
		t.Fatal("successful RunNow must persist a digest")
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted = %v, want exactly one event on a genuinely-new digest", emitted)
	}
	successEvents := 0
	for _, e := range auditLog.events {
		if e.Type == audit.EventLearningDigest {
			successEvents++
		}
	}
	if successEvents != 1 {
		t.Fatalf("audit success events = %d, want 1", successEvents)
	}
}

// extractWindowFromPrompt pulls the "since=... until=..." pair buildPrompt
// embeds, so a fake summarizer can echo the exact window it was given
// without hardcoding it — the packet's actual window shifts run to run as
// windowFor advances from the previous digest's Until.
func extractWindowFromPrompt(t *testing.T, prompt string) (since, until string) {
	t.Helper()
	const marker = "Window: since="
	_, rest, ok := strings.Cut(prompt, marker)
	if !ok {
		t.Fatalf("prompt missing window marker: %q", prompt)
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		t.Fatalf("could not parse window from prompt tail: %q", rest)
	}
	since = fields[0]
	until = strings.TrimPrefix(fields[1], "until=")
	return since, until
}

func TestStorePutDedupsIdenticalReportOverSameWindow(t *testing.T) {
	since, until := testWindow()
	recs := []stats.RunRecord{{TaskID: "A", Timestamp: since.Add(time.Hour)}}
	evts := []audit.Event{{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: since.Add(time.Hour)}}
	pkt := buildPacket(recs, evts, abtest.Config{}, since, until, nil)

	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	d, err := validateDigest(rd, pkt)
	if err != nil {
		t.Fatalf("validateDigest returned error: %v", err)
	}
	d.SchemaVersion = SchemaVersion
	d.ReportDigest = pkt.ReportDigest
	d.GeneratedAt = since

	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(store) returned error: %v", err)
	}

	stored1, err := store.Put(d)
	if err != nil {
		t.Fatalf("first Put returned error: %v", err)
	}
	if !stored1 {
		t.Fatal("first Put over a fresh window/report must store")
	}

	// A repeated tick over the identical window and unchanged underlying data
	// produces the same ReportDigest content hash and therefore the same
	// Key() — Put must dedup rather than writing a duplicate row.
	pkt2 := buildPacket(recs, evts, abtest.Config{}, since, until, nil)
	if pkt2.ReportDigest != pkt.ReportDigest {
		t.Fatalf("unchanged underlying data produced a different ReportDigest: %q != %q", pkt2.ReportDigest, pkt.ReportDigest)
	}
	d2 := d
	d2.ReportDigest = pkt2.ReportDigest
	stored2, err := store.Put(d2)
	if err != nil {
		t.Fatalf("second Put returned error: %v", err)
	}
	if stored2 {
		t.Fatal("second Put over the same window/report must dedup, not store again")
	}
}

func TestRunNowRejectsNonClaudeProvider(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	since := now.AddDate(0, 0, -7)
	in := since.Add(time.Hour)

	summarizer := func(_ context.Context, _ string, _ llmexec.Options) (llmexec.Result, error) {
		return llmexec.Result{Provider: "codex", Text: "{}"}, nil
	}

	svc := newTestService(t, Deps{
		Cfg:        config.LearningDigestConfig{MinRuns: 1, MinLandings: 1, WindowDays: 7, MaxWindowDays: 30},
		Stats:      fakeStats{records: []stats.RunRecord{{TaskID: "A", Timestamp: in}}},
		Audit:      fakeAudit{events: []audit.Event{{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in}}},
		Now:        func() time.Time { return now },
		Summarizer: summarizer,
	})

	if _, err := svc.RunNow(context.Background()); err == nil {
		t.Fatal("RunNow accepted a non-claude summarizer response")
	}
}

func TestClaudeOnlyGateMasksNonClaudeProviders(t *testing.T) {
	g := claudeOnlyGate{base: nil}
	if !g.IsHealthy("claude") {
		t.Fatal("claudeOnlyGate must report claude healthy when no base gate is set")
	}
	if g.IsHealthy("codex") || g.IsHealthy("copilot") {
		t.Fatal("claudeOnlyGate must mask every non-claude provider unhealthy")
	}
}

type recordingAuditLog struct {
	events []audit.Event
}

func (r *recordingAuditLog) Log(e audit.Event) error {
	r.events = append(r.events, e)
	return nil
}
