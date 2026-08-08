package monitor

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestMergeIncidentChangePreservesStrongestTransition(t *testing.T) {
	if got := mergeIncidentChange(IncidentReopened, IncidentUnchanged); got != IncidentReopened {
		t.Fatalf("merge = %q, want reopened", got)
	}
	if got := mergeIncidentChange(IncidentExpanded, IncidentOpened); got != IncidentOpened {
		t.Fatalf("merge = %q, want opened", got)
	}
}

func TestObserveIncidentsPreservesFleetScopeForUnscopedTask(t *testing.T) {
	store := newTestIncidentStore(t)
	svc := &Service{incidents: store}
	anoms := []Anomaly{{Kind: KindUntriaged, TaskID: "t", DetectedAt: time.Now().UTC()}}
	svc.observeIncidents([]task.Task{{ID: "t"}}, anoms)
	if anoms[0].IncidentScope != "fleet" {
		t.Fatalf("unscoped task incident scope = %q, want fleet", anoms[0].IncidentScope)
	}
}

func newTestIncidentStore(t *testing.T) *IncidentStore {
	t.Helper()
	store, err := NewIncidentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestControlPlaneAnomaliesConsumeTypedLeaseAndReconciliationResults(t *testing.T) {
	now := time.Now().UTC()
	got := controlPlaneAnomalies([]audit.Event{
		{Timestamp: now, Type: audit.EventAttemptLeasesReconciled, Data: map[string]any{"count": 2}},
		{Timestamp: now, Type: audit.EventReconciliationDecided, TaskID: "work-task-opaque", Data: map[string]any{"action": "repair", "project_scope": "work-opaque", "confidential": true}},
		{Timestamp: now, Type: audit.EventReconciliationDecided, Data: map[string]any{"action": "advance"}},
	})
	if len(got) != 2 {
		t.Fatalf("typed control-plane anomalies = %+v", got)
	}
	leaseCause := rootCauseFor(got[0], "fleet", "g")
	if leaseCause.FailureCode != "orphan_attempt_lease" || leaseCause.Capability != "attempt-leases" {
		t.Fatalf("lease cause = %+v", leaseCause)
	}
	if !got[1].Confidential || got[1].IncidentScope != "work-opaque" || got[1].IncidentTaskID != "work-task-opaque" {
		t.Fatalf("reconciliation confidentiality projection = %+v", got[1])
	}
}

func TestRootCauseFingerprintIgnoresTaskAndVolatileEvidence(t *testing.T) {
	cause := RootCause{FailureCode: "lost_agent", Component: "agent", Capability: "process-lifecycle", ProjectScope: "p", ConfigGeneration: "g"}
	a := Anomaly{Kind: KindLostAgent, TaskID: "one", Evidence: map[string]any{"status": string(taskstatus.Todo), "dwell_h": 1.0}}
	b := Anomaly{Kind: KindLostAgent, TaskID: "two", Evidence: map[string]any{"status": string(taskstatus.InProgress), "dwell_h": 99.0}}
	if got, want := RootCauseFingerprint(rootCauseFor(a, "p", "g")), RootCauseFingerprint(rootCauseFor(b, "p", "g")); got != want || got != RootCauseFingerprint(cause) {
		t.Fatalf("volatile/task fields changed root fingerprint: got %q want %q", got, want)
	}
	changed := cause
	changed.Capability = "provider_capacity"
	if RootCauseFingerprint(changed) == RootCauseFingerprint(cause) {
		t.Fatal("material capability change did not rekey incident")
	}
}

func TestMonitorConfigGenerationIsCauseSpecific(t *testing.T) {
	cfg := config.MonitorConfig{LostAgentMinutes: 5, StuckHumanHours: 2, IssueCooldownMinutes: 10}
	base := monitorConfigGeneration(KindLostAgent, cfg)
	cfg.IssueCooldownMinutes = 99
	if got := monitorConfigGeneration(KindLostAgent, cfg); got != base {
		t.Fatal("unrelated publication cooldown rekeyed lost-agent cause")
	}
	cfg.LostAgentMinutes++
	if got := monitorConfigGeneration(KindLostAgent, cfg); got == base {
		t.Fatal("relevant detector threshold did not rekey cause")
	}
}

func TestIncidentStoreCoalescesAcrossTasksAndProcesses(t *testing.T) {
	dir := t.TempDir()
	one, err := NewIncidentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewIncidentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	cause := RootCause{FailureCode: "lost_agent", Component: "agent", Capability: "process-lifecycle", ProjectScope: "p", ConfigGeneration: "g"}
	var wg sync.WaitGroup
	for i, pair := range []struct {
		store *IncidentStore
		task  string
	}{{one, "t1"}, {two, "t2"}} {
		wg.Add(1)
		go func(i int, pair struct {
			store *IncidentStore
			task  string
		}) {
			defer wg.Done()
			_, _, observeErr := pair.store.Observe(Anomaly{Kind: KindLostAgent, TaskID: pair.task, DetectedAt: now.Add(time.Duration(i) * time.Second)}, cause, pair.task)
			if observeErr != nil {
				t.Errorf("Observe: %v", observeErr)
			}
		}(i, pair)
	}
	wg.Wait()
	in, ok, err := one.Get(RootCauseFingerprint(cause))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if in.AffectedTaskCount != 2 || len(in.AffectedTaskIDs) != 2 {
		t.Fatalf("affected tasks = count:%d ids:%v, want two", in.AffectedTaskCount, in.AffectedTaskIDs)
	}
}

func TestIncidentHealthyGraceRequiresCoverageAndProvesAttempt(t *testing.T) {
	store, err := NewIncidentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	cause := RootCause{FailureCode: "lost_agent", Component: "agent", Capability: "process-lifecycle", ProjectScope: "p", ConfigGeneration: "g"}
	a := Anomaly{Kind: KindLostAgent, DetectedAt: base}
	in, _, err := store.Observe(a, cause, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemediation(in.Fingerprint, "reset", "attempted", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, _, _ := store.Get(in.Fingerprint)
	if pending.FirstContainedAt != nil {
		t.Fatal("unproven remediation was counted as containment")
	}

	// Unknown/uncovered is not a healthy observation and cannot start grace.
	closed, err := store.ReconcileHealthy(nil, map[string]bool{"p": true}, map[string]bool{}, map[string]string{"lost_agent": "g"}, base.Add(2*time.Minute), time.Minute, time.Minute)
	if err != nil || len(closed) != 0 {
		t.Fatalf("unknown observation closed incident: %v %v", closed, err)
	}
	got, _, _ := store.Get(in.Fingerprint)
	if got.HealthySince != nil {
		t.Fatal("unknown observation advanced healthy grace")
	}

	coverage := map[string]bool{"lost_agent": true}
	_, err = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, map[string]string{"lost_agent": "g"}, base.Add(3*time.Minute), time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	closed, err = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, map[string]string{"lost_agent": "g"}, base.Add(4*time.Minute), time.Minute, time.Minute)
	if err != nil || len(closed) != 1 {
		t.Fatalf("healthy grace close = %v err=%v", closed, err)
	}
	if closed[0].RemediationAttempts[0].Result != "observed_success" || closed[0].RemediationAttempts[0].ObservedAt == nil {
		t.Fatalf("attempt was not proven by later health: %+v", closed[0].RemediationAttempts)
	}
	if closed[0].FirstContainedAt == nil || !closed[0].FirstContainedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("certified containment time = %v", closed[0].FirstContainedAt)
	}
}

func TestIncidentObservationFailsPriorAttemptAndOnlyLaterRepairSucceeds(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Date(2026, 8, 8, 2, 0, 0, 0, time.UTC)
	cause := RootCause{FailureCode: "lost_agent", Component: "agent", Capability: "process-lifecycle", ProjectScope: "p", ConfigGeneration: "g"}
	in, _, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base}, cause, "t")
	if err := store.RecordRemediation(in.Fingerprint, "reset", "failed", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemediation(in.Fingerprint, "reset", "attempted", base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base.Add(3 * time.Minute)}, cause, "t"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemediation(in.Fingerprint, "reset", "attempted", base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	coverage := map[string]bool{"lost_agent": true}
	generation := map[string]string{"lost_agent": "g"}
	_, _ = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(5*time.Minute), time.Minute, time.Minute)
	_, _ = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(6*time.Minute), time.Minute, time.Minute)
	got, _, _ := store.Get(in.Fingerprint)
	if got.RemediationAttempts[0].Result != "failed" || got.RemediationAttempts[0].ObservedAt != nil {
		t.Fatalf("API failure was misclassified as containment evidence: %+v", got.RemediationAttempts[0])
	}
	if got.RemediationAttempts[1].Result != "observed_failure" || got.RemediationAttempts[1].ObservedAt == nil {
		t.Fatalf("recurrence did not disprove prior attempt: %+v", got.RemediationAttempts[1])
	}
	if got.RemediationAttempts[2].Result != "observed_success" || got.RemediationAttempts[2].ObservedAt == nil {
		t.Fatalf("healthy grace did not prove latest attempt: %+v", got.RemediationAttempts[2])
	}
}

func TestIncidentRecurrenceReopensSameHistoryAfterGrace(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	cause := RootCause{FailureCode: "untriaged", Component: "triage", Capability: "classification", ProjectScope: "p", ConfigGeneration: "g"}
	in, _, _ := store.Observe(Anomaly{Kind: KindUntriaged, DetectedAt: base}, cause, "t1")
	coverage := map[string]bool{"untriaged": true}
	generation := map[string]string{"untriaged": "g"}
	_, _ = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(time.Minute), time.Minute, 2*time.Minute)
	_, _ = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(2*time.Minute), time.Minute, 2*time.Minute)
	_, suppressed, _ := store.Observe(Anomaly{Kind: KindUntriaged, DetectedAt: base.Add(3 * time.Minute)}, cause, "t2")
	if suppressed != IncidentUnchanged {
		t.Fatalf("reopen inside suppression = %q, want unchanged", suppressed)
	}
	reopened, change, _ := store.Observe(Anomaly{Kind: KindUntriaged, DetectedAt: base.Add(5 * time.Minute)}, cause, "t2")
	if change != IncidentReopened || reopened.Fingerprint != in.Fingerprint || reopened.RecurrenceCount != 1 || reopened.FirstSeen != in.FirstSeen {
		t.Fatalf("reopened incident lost identity/history: change=%q %+v", change, reopened)
	}
}

func TestIncidentStoreConfigRekeyLetsPriorGenerationResolve(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC)
	cause := RootCause{FailureCode: "lost_agent", Component: "agent", Capability: "lifecycle", ProjectScope: "p", ConfigGeneration: "old"}
	in, _, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base}, cause, "t")
	coverage := map[string]bool{"lost_agent": true}
	closed, err := store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, map[string]string{"lost_agent": "generation-next"}, base.Add(time.Minute), time.Minute, 0)
	if err != nil || len(closed) != 1 || closed[0].Fingerprint != in.Fingerprint || closed[0].SupersededAt == nil || closed[0].ResolvedAt != nil {
		t.Fatalf("old generation was stranded: closed=%+v err=%v", closed, err)
	}
}

func TestIncidentStoreLinkPreservesUnspecifiedHistory(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Now().UTC()
	cause := RootCause{FailureCode: "lost_agent", ProjectScope: "p", ConfigGeneration: "g"}
	in, _, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base}, cause, "t")
	if err := store.Link(in.Fingerprint, "issue", "pr", []int{3, 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(in.Fingerprint, "issue-new", "", nil); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(in.Fingerprint)
	if got.IssueURL != "issue-new" || got.PRURL != "pr" || fmt.Sprint(got.DuplicateIssues) != "[2 3]" {
		t.Fatalf("link history erased: %+v", got)
	}
}

func TestIncidentStoreFanoutBecomesBoundedUnknownWithoutInflation(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Now().UTC()
	cause := RootCause{FailureCode: "lost_agent", ProjectScope: "p", ConfigGeneration: "g"}
	var fp string
	for i := range maxAffectedTasks {
		in, _, err := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base.Add(time.Duration(i) * time.Second)}, cause, fmt.Sprintf("t-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		fp = in.Fingerprint
	}
	before, _, _ := store.Get(fp)
	_, change, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base.Add(time.Hour)}, cause, "t-overflow")
	after, _, _ := store.Get(fp)
	if !after.AffectedTaskOverflow || after.AffectedTaskCount != maxAffectedTasks || after.Revision != before.Revision+1 || change != IncidentExpanded {
		t.Fatalf("bounded fanout drifted: before=%+v after=%+v change=%s", before, after, change)
	}
	_, repeatChange, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base.Add(2 * time.Hour)}, cause, "t-overflow")
	repeated, _, _ := store.Get(fp)
	if repeatChange != IncidentUnchanged || repeated.Revision != after.Revision {
		t.Fatalf("unknown fanout kept producing revisions: change=%s revision=%d want=%d", repeatChange, repeated.Revision, after.Revision)
	}
}

func TestIncidentHealthyProofResolvesAllPendingRemediations(t *testing.T) {
	store := newTestIncidentStore(t)
	base := time.Now().UTC()
	cause := RootCause{FailureCode: "lost_agent", ProjectScope: "p", ConfigGeneration: "g"}
	in, _, _ := store.Observe(Anomaly{Kind: KindLostAgent, DetectedAt: base}, cause, "t")
	_ = store.RecordRemediation(in.Fingerprint, "reset", "attempted", base.Add(time.Minute))
	_ = store.RecordRemediation(in.Fingerprint, "reset", "attempted", base.Add(2*time.Minute))
	coverage := map[string]bool{"lost_agent": true}
	generation := map[string]string{"lost_agent": "g"}
	_, _ = store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(3*time.Minute), time.Minute, 0)
	closed, err := store.ReconcileHealthy(nil, map[string]bool{"p": true}, coverage, generation, base.Add(4*time.Minute), time.Minute, 0)
	if err != nil || len(closed) != 1 {
		t.Fatalf("reconcile: closed=%v err=%v", closed, err)
	}
	for _, attempt := range closed[0].RemediationAttempts {
		if attempt.Result != "observed_success" || attempt.ObservedAt == nil {
			t.Fatalf("pending attempt not resolved: %+v", attempt)
		}
	}
	if closed[0].FirstContainedAt == nil || !closed[0].FirstContainedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("containment should begin after the last successful action: %v", closed[0].FirstContainedAt)
	}
}
