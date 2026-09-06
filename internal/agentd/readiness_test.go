package agentd

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/executioncontract"
)

func TestSpoolBacklogReportsOnlyCountsAndOldestAge(t *testing.T) {
	d := registrationTestDaemon(t, "http://127.0.0.1:1")
	now := time.Now()
	if err := d.spool.update(func(state *durableState) error {
		state.Events["run-a"] = []executioncontract.EventEnvelope{{ObservedAt: now.Add(-42 * time.Second)}}
		state.Events["run-b"] = []executioncontract.EventEnvelope{{ObservedAt: now.Add(time.Minute)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events, artifacts, age := d.spool.backlog(now)
	if events != 2 || artifacts != 0 || age != 42 {
		t.Fatalf("backlog=%d/%d/%d", events, artifacts, age)
	}
}

func TestReadinessRecoversAfterStoragePressure(t *testing.T) {
	d := registrationTestDaemon(t, "http://127.0.0.1:1")
	d.cfg.MinDiskFreeBytes = math.MaxInt64
	if got := d.readiness(); got != "storage_pressure" {
		t.Fatalf("readiness = %q", got)
	}
	if !slices.Contains(d.runtimeCapabilities(t.Context()), "readiness=storage_pressure") {
		t.Fatal("pressure was not advertised to the leader")
	}
	d.cfg.MinDiskFreeBytes = 1
	if got := d.readiness(); got != "ready" {
		t.Fatalf("readiness after recovery = %q", got)
	}
	for _, root := range []string{d.cfg.StateRoot, d.cfg.WorkspaceRoot} {
		matches, err := filepath.Glob(filepath.Join(root, ".agentd-readiness-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("probe left files: %v, %v", matches, err)
		}
	}
}

func TestStorageProbeRefusesFileAsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeStorage(path); err == nil {
		t.Fatal("probe accepted an unwritable storage root")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "untouched" {
		t.Fatal("storage probe modified an existing file")
	}
}

func TestQueuedStartRechecksReadinessBeforePreparation(t *testing.T) {
	d := registrationTestDaemon(t, "http://127.0.0.1:1")
	d.cfg.MinDiskFreeBytes = math.MaxInt64
	spec := validSpec("run-unready")
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.start(t.Context(), executioncontract.CommandEnvelope{RunID: spec.RunID, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	events := d.spool.snapshot().Events[spec.RunID]
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	var terminal struct {
		AdmissionDeferred bool `json:"admissionDeferred"`
	}
	if err := json.Unmarshal(events[0].Payload, &terminal); err != nil || !terminal.AdmissionDeferred {
		t.Fatalf("missing infrastructure disposition: %s, %v", events[0].Payload, err)
	}
	if entries, err := os.ReadDir(d.cfg.WorkspaceRoot); err != nil || len(entries) != 0 {
		t.Fatalf("unready start prepared a checkout: %v, %v", entries, err)
	}
}

func TestArtifactFailurePreservesExecutionFailure(t *testing.T) {
	d := registrationTestDaemon(t, "http://127.0.0.1:1")
	runID := "run-artifact-failure"
	a := &agent.Agent{ID: "agent-artifact-failure"}
	original := errors.New("provider rejected request")
	a.SetExitErr(original)
	d.agentRuns[a.ID], d.runAgents[runID] = runID, a.ID
	if err := d.spool.update(func(state *durableState) error {
		state.RunSpecs[runID] = validSpec(runID)
		state.RunAgents[runID] = a.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// No checkout exists: artifact collection will fail independently of the
	// provider error. The terminal event must retain both outcomes durably.
	d.completeAgent(a)
	if !errors.Is(a.GetExitErr(), original) {
		t.Fatalf("provider error replaced by %v", a.GetExitErr())
	}
	events := d.spool.snapshot().Events[runID]
	if len(events) != 1 || events[0].Type != executioncontract.EventTerminal {
		t.Fatalf("terminal events = %+v", events)
	}
	var payload struct {
		State         string `json:"state"`
		Error         string `json:"error"`
		ArtifactState string `json:"artifactState"`
		ArtifactError string `json:"artifactError"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error != original.Error() || payload.State != "failed" || payload.ArtifactState != "failed" || payload.ArtifactError != "artifact collection failed" {
		t.Fatalf("terminal lost failure information: %+v", payload)
	}
}
