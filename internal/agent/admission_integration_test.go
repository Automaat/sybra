package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
)

type recordingAttemptAdmission struct {
	mu          sync.Mutex
	acquired    []AttemptIntent
	bound       []AttemptBinding
	heartbeats  int
	completed   []string
	adopted     int
	adoptErr    error
	nextVersion uint64
	existing    bool
}

func (a *recordingAttemptAdmission) Acquire(_ context.Context, intent AttemptIntent) (AttemptLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acquired = append(a.acquired, intent)
	a.nextVersion++
	return AttemptLease{ID: "lease-" + intent.IntentID, Version: a.nextVersion, Existing: a.existing}, nil
}

func (a *recordingAttemptAdmission) Bind(_ context.Context, _ AttemptLease, binding AttemptBinding) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bound = append(a.bound, binding)
	return nil
}

func (a *recordingAttemptAdmission) Heartbeat(context.Context, AttemptLease, time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.heartbeats++
	return nil
}

func (a *recordingAttemptAdmission) Complete(_ context.Context, _ AttemptLease, outcome string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.completed = append(a.completed, outcome)
	return nil
}

func (a *recordingAttemptAdmission) Adopt(_ context.Context, _ AttemptIntent, lease AttemptLease, _ AttemptBinding) (AttemptLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adopted++
	lease.Version++
	return lease, a.adoptErr
}

func TestRunContextAttemptAdmissionLifecycleOnStartFailure(t *testing.T) {
	m, _ := newTestManager(t)
	recorder := &recordingAttemptAdmission{}
	m.attemptAdmission = recorder
	cfg := RunConfig{
		Mode: "headless", Role: RoleMonitor, Provider: providerid.Claude, Prompt: "same prompt", Dir: t.TempDir(), ReadOnlyDir: true,
		BeforeStart: func(string) error { return errors.New("fixture rejects start") },
	}

	if _, err := m.RunContext(context.Background(), cfg); err == nil {
		t.Fatal("RunContext unexpectedly accepted rejected start")
	}
	if _, err := m.RunContext(context.Background(), cfg); err == nil {
		t.Fatal("second RunContext unexpectedly accepted rejected start")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.acquired) != 2 {
		t.Fatalf("acquired = %d, want 2", len(recorder.acquired))
	}
	if recorder.acquired[0].IntentID == "" || recorder.acquired[0].IntentID == recorder.acquired[1].IntentID {
		t.Fatalf("fallback intent ids = %q, %q; want distinct non-empty dispatch ids", recorder.acquired[0].IntentID, recorder.acquired[1].IntentID)
	}
	if recorder.acquired[0].Access != AttemptAccessObserve || !recorder.acquired[0].CapabilityCertified {
		t.Fatalf("intent = %+v, want certified observer", recorder.acquired[0])
	}
	if len(recorder.bound) != 2 || recorder.bound[0].AgentID == "" {
		t.Fatalf("bindings = %+v, want allocated agent identities", recorder.bound)
	}
	if len(recorder.completed) != 2 || recorder.completed[0] != "before_start_failed" || recorder.completed[1] != "before_start_failed" {
		t.Fatalf("completed = %v, want two before_start_failed releases", recorder.completed)
	}
}

func TestRunContextAttemptReplayDoesNotBindOrStart(t *testing.T) {
	m, _ := newTestManager(t)
	recorder := &recordingAttemptAdmission{existing: true}
	m.attemptAdmission = recorder
	beforeStartCalls := 0
	_, err := m.RunContext(context.Background(), RunConfig{
		Mode: "headless", Role: RoleMonitor, Provider: providerid.Claude, Prompt: "replay",
		Dir: t.TempDir(), IntentID: "effect-1",
		BeforeStart: func(string) error { beforeStartCalls++; return nil },
	})
	if !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("RunContext replay error = %v, want ErrAttemptConflict", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.acquired) != 1 || len(recorder.bound) != 0 || len(recorder.completed) != 0 {
		t.Fatalf("replay lifecycle acquired=%d bound=%d completed=%d, want 1/0/0", len(recorder.acquired), len(recorder.bound), len(recorder.completed))
	}
	if beforeStartCalls != 0 {
		t.Fatalf("BeforeStart calls = %d, replay reached start path", beforeStartCalls)
	}
}

func TestRegisterRunningAgentHardProviderCapConcurrent(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.maxConcurrent = 100
	m.maxInFlightPerProvider = 3
	m.mu.Unlock()

	const contenders = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := make([]*Agent, 0, 3)
	errs := make([]error, 0, contenders-3)
	for i := range contenders {
		wg.Go(func() {
			a := &Agent{ID: fmt.Sprintf("cap-%d", i), Provider: providerid.Claude, done: make(chan struct{})}
			err := m.registerRunningAgent(a, RunConfig{}, func() {})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				admitted = append(admitted, a)
			} else {
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()
	if len(admitted) != 3 {
		t.Fatalf("admitted = %d, want hard provider cap 3", len(admitted))
	}
	for _, err := range errs {
		if !errors.Is(err, ErrProviderCapacityReached) {
			t.Fatalf("rejection = %v, want ErrProviderCapacityReached", err)
		}
	}
	for _, a := range admitted {
		m.markAgentDone(context.Background(), a)
	}
}

type fixedSurvivalRegistry struct {
	mu      sync.Mutex
	records []Record
	saved   []Record
}

func (r *fixedSurvivalRegistry) Save(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, rec)
	return nil
}
func (r *fixedSurvivalRegistry) List() ([]Record, error) { return r.records, nil }
func (r *fixedSurvivalRegistry) Delete(string) error     { return nil }

func TestReattachRequiresAdmissionAdoptionBeforeExposure(t *testing.T) {
	m, _ := newTestManager(t)
	wantErr := errors.New("lease owned by another node")
	recorder := &recordingAttemptAdmission{adoptErr: wantErr}
	m.attemptAdmission = recorder
	m.reg = &fixedSurvivalRegistry{records: []Record{{
		ID: "reattach-lease", TaskID: "task-1", Mode: "headless", Provider: providerid.Claude,
		PID: os.Getpid(), ProcStartedAt: processStartString(context.Background(), os.Getpid()),
		AttemptIntentID: "intent-1", AttemptAccess: AttemptAccessMutate,
		AttemptLeaseID: "lease-1", AttemptVersion: 2,
	}}}
	m.surviveRestart = true

	if got := m.ReattachAllContext(context.Background()); len(got) != 0 {
		t.Fatalf("reattached = %d, want none when durable adoption fails", len(got))
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.adopted != 1 {
		t.Fatalf("adopt calls = %d, want 1", recorder.adopted)
	}
	if m.RunningCount() != 0 {
		t.Fatalf("running count = %d, attempt was exposed before adoption", m.RunningCount())
	}
}

func TestReattachPersistsTransferredLeaseBeforeExposure(t *testing.T) {
	m, _ := newTestManager(t)
	recorder := &recordingAttemptAdmission{}
	m.attemptAdmission = recorder
	registry := &fixedSurvivalRegistry{records: []Record{{
		ID: "reattach-transferred", TaskID: "task-1", Mode: "headless", Provider: providerid.Claude,
		PID: os.Getpid(), ProcStartedAt: processStartString(context.Background(), os.Getpid()),
		AttemptIntentID: "intent-1", AttemptAccess: AttemptAccessMutate,
		AttemptLeaseID: "lease-1", AttemptVersion: 2,
	}}}
	m.reg = registry
	m.surviveRestart = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := m.ReattachAllContext(ctx)
	if len(got) != 1 {
		t.Fatalf("reattached = %d, want 1", len(got))
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.saved) != 1 || registry.saved[0].AttemptVersion != 3 {
		t.Fatalf("saved records = %+v, want transferred lease version 3", registry.saved)
	}
	if got[0].attemptLease.Version != 3 {
		t.Fatalf("exposed lease version = %d, want 3", got[0].attemptLease.Version)
	}
}

func TestReattachBootstrapsLeaseForLegacyLiveRecord(t *testing.T) {
	m, _ := newTestManager(t)
	recorder := &recordingAttemptAdmission{}
	m.attemptAdmission = recorder
	registry := &fixedSurvivalRegistry{records: []Record{{
		ID: "legacy-live", TaskID: "task-1", Mode: "headless", Provider: providerid.Claude,
		PID: os.Getpid(), ProcStartedAt: processStartString(context.Background(), os.Getpid()),
		CWD: t.TempDir(),
	}}}
	m.reg = registry
	m.surviveRestart = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := m.ReattachAllContext(ctx)
	if len(got) != 1 {
		t.Fatalf("reattached = %d, want legacy process fenced and exposed", len(got))
	}
	recorder.mu.Lock()
	if len(recorder.acquired) != 1 || len(recorder.bound) != 1 || recorder.adopted != 0 {
		t.Fatalf("legacy lifecycle acquired=%d bound=%d adopted=%d, want 1/1/0", len(recorder.acquired), len(recorder.bound), recorder.adopted)
	}
	if recorder.acquired[0].IntentID != "legacy-registry:legacy-live" || recorder.acquired[0].Access != AttemptAccessMutate {
		t.Fatalf("legacy intent = %+v", recorder.acquired[0])
	}
	recorder.mu.Unlock()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.saved) != 1 || registry.saved[0].AttemptLeaseID == "" || registry.saved[0].AttemptIntentID == "" {
		t.Fatalf("saved legacy record = %+v, want durable attempt identity", registry.saved)
	}
}

func TestExplicitStartFailureOutcomeWinsTerminalBackstop(t *testing.T) {
	m, _ := newTestManager(t)
	recorder := &recordingAttemptAdmission{}
	m.attemptAdmission = recorder
	a := &Agent{
		ID: "start-failure", Provider: providerid.Claude, State: StateRunning,
		done: make(chan struct{}), attemptLease: AttemptLease{ID: "lease", Version: 1},
	}
	if err := m.registerRunningAgent(a, RunConfig{}, func() {}); err != nil {
		t.Fatal(err)
	}
	m.completeAttempt(t.Context(), a, "start_failed")
	m.markAgentDone(t.Context(), a)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.completed) != 1 || recorder.completed[0] != "start_failed" {
		t.Fatalf("completed outcomes = %v, want [start_failed]", recorder.completed)
	}
}
