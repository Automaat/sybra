package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentworkspace"
	agentevents "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestDaemonReregistersAfterSessionRejection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	control := workercontrol.New(dbtest.SQLite(t))
	controlHandler := control.Handler()
	var registrations atomic.Int32
	var rejected atomic.Bool
	replacementCommitted := make(chan struct{})
	releaseLostResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/register" {
			attempt := registrations.Add(1)
			if attempt == 2 {
				// Commit the replacement but hide its response. The daemon must
				// retry the same durable registration identity and discover it.
				recorder := httptest.NewRecorder()
				controlHandler.ServeHTTP(recorder, r)
				close(replacementCommitted)
				<-releaseLostResponse
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			controlHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/worker/v1/commands" && registrations.Load() == 1 && rejected.CompareAndSwap(false, true) {
			diagnostics, err := control.Diagnostics(r.Context())
			if err != nil || activeSession(diagnostics) == "" {
				http.Error(w, "missing active session", http.StatusInternalServerError)
				return
			}
			spec, command := recoveryStartContract(t)
			if _, err := control.Enqueue(r.Context(), activeSession(diagnostics), &spec, command); err != nil {
				http.Error(w, "enqueue recovery fixture", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": workercontrol.ErrLeaseExpired.Error()})
			return
		}
		controlHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := Config{
		LeaderURL: server.URL, TokenEnv: "AGENTD_TEST_TOKEN", NodeID: "node-recover", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20, LeaseSeconds: 30, PollSeconds: 1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemon, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	select {
	case <-replacementCommitted:
		if err := daemon.spool.update(func(state *durableState) error {
			state.OutputCounts["during-registration-retry"]++
			state.LastCommandAck++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		close(releaseLostResponse)
	case err := <-done:
		t.Fatalf("daemon exited before replacement commit: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement registration was not attempted")
	}

	var diagnostics []workercontrol.Diagnostics
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("daemon exited during session recovery: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		diagnostics, err = control.Diagnostics(t.Context())
		state := daemon.spool.snapshot()
		if registrations.Load() == 3 && err == nil && activeSession(diagnostics) == state.SessionID {
			break
		}
	}
	if registrations.Load() != 3 {
		t.Fatalf("registrations = %d, want initial, lost response, and idempotent retry", registrations.Load())
	}
	state := daemon.spool.snapshot()
	if state.SessionID == "" || state.LastCommandAck != 1 {
		t.Fatalf("recovered durable session = %+v", state)
	}
	if err != nil || activeSession(diagnostics) != state.SessionID {
		t.Fatalf("diagnostics = %+v, %v; want recovered session %s", diagnostics, err, state.SessionID)
	}
}

func TestFreshRegistrationRetryResetsObsoleteCommandCursor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/register" {
			http.NotFound(w, r)
			return
		}
		var request workercontrol.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch attempts.Add(1) {
		case 1:
			if request.ResumeSessionID != "stale-session" || request.LastCommandAck != 7 {
				http.Error(w, "expected exact stale resume", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": workercontrol.ErrStaleSession.Error()})
		case 2:
			if request.ResumeSessionID != "" || request.LastCommandAck != 0 {
				http.Error(w, "expected fresh registration", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusInternalServerError) // committed response lost
		case 3:
			if request.ResumeSessionID != "" || request.LastCommandAck != 0 {
				http.Error(w, "fresh retry changed identity", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(workercontrol.Session{SessionID: "fresh-session", State: "active", LastCommandAck: 0})
		default:
			http.Error(w, "unexpected registration", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	daemon, err := New(t.Context(), Config{
		LeaderURL: server.URL, TokenEnv: "AGENTD_TEST_TOKEN", NodeID: "node-fresh-retry", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20, LeaseSeconds: 30, PollSeconds: 1,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.spool.update(func(state *durableState) error {
		state.SessionID, state.LastCommandAck = "stale-session", 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.register(t.Context()); err == nil {
		t.Fatal("lost fresh-registration response unexpectedly succeeded")
	}
	if err := daemon.register(t.Context()); err != nil {
		t.Fatalf("retry fresh registration: %v", err)
	}
	state := daemon.spool.snapshot()
	if state.SessionID != "fresh-session" || state.LastCommandAck != 0 || state.RegistrationID != "" {
		t.Fatalf("fresh registration state = %+v", state)
	}
}

func recoveryStartContract(t *testing.T) (executioncontract.RunSpec, executioncontract.CommandEnvelope) {
	t.Helper()
	now := time.Now().UTC()
	spec := executioncontract.RunSpec{
		Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", RunID: "run-recovery",
		EffectID: "effect-recovery", IdempotencyKey: "start:effect-recovery",
		Fence: executioncontract.GenerationFence{TaskID: "task-recovery", TaskGeneration: 1, WorkflowID: "ship", WorkflowGeneration: 1, StepID: "implement"},
		Role:  "implementation", Provider: executioncontract.ProviderIntent{Provider: "test", Model: "test-model"},
		Prompt: executioncontract.Prompt{Text: "recovery fixture"}, Deadline: now.Add(time.Hour),
		Workspace: executioncontract.Workspace{RepositoryID: "repo", BaseSHA: strings.Repeat("a", 40), BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree}},
	}
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	return spec, executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "leader-test", CommandID: "command:recovery",
		RunID: spec.RunID, IdempotencyKey: spec.IdempotencyKey, Type: executioncontract.CommandStart, SentAt: now, Payload: payload,
	}
}

func activeSession(diagnostics []workercontrol.Diagnostics) string {
	var active string
	for _, item := range diagnostics {
		if item.State != "active" {
			continue
		}
		if active != "" {
			return ""
		}
		active = item.SessionID
	}
	return active
}

func TestDaemonExecutesFakeProviderThroughDurableProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	repo, baseSHA := testRepository(t, root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(bin, providerid.Claude)
	script := `#!/bin/sh
if [ -n "${AGENTD_TEST_TOKEN:-}" ] || [ -n "${AGENTD_UNUSED_RUN_SECRET:-}" ]; then
  printf '%s\n' '{"type":"result","subtype":"error","result":"leader credential leaked"}'
  exit 9
fi
printf '%s\n' '{"type":"system","session_id":"agentd-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"remote output"}]}}'
printf '%s\n' '{"type":"result","result":"done","session_id":"agentd-session","total_cost_usd":0.01}'
`
	if err := os.WriteFile(claude, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	t.Setenv("AGENTD_UNUSED_RUN_SECRET", "scoped-secret")

	database := dbtest.SQLite(t)
	control := workercontrol.New(database)
	server := httptest.NewServer(control.Handler())
	t.Cleanup(server.Close)
	cfg := Config{
		LeaderURL: server.URL, TokenEnv: "AGENTD_TEST_TOKEN", NodeID: "node-test", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20, LeaseSeconds: 30, PollSeconds: 1,
		SecretEnv:    map[string]string{"run/unused/input": "AGENTD_UNUSED_RUN_SECRET"},
		Repositories: map[string]string{"repo": repo},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemon, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()

	var diagnostics workercontrol.Diagnostics
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var all []workercontrol.Diagnostics
		all, err = control.Diagnostics(t.Context())
		if err == nil && len(all) > 0 {
			diagnostics = all[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if diagnostics.SessionID == "" {
		t.Fatalf("daemon did not register: %v", err)
	}
	spec := validSpec("run-agentd")
	spec.Workspace.BaseSHA = baseSHA
	payload, _ := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	command := executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "cmd-start",
		RunID: spec.RunID, IdempotencyKey: "command-start", Type: executioncontract.CommandStart,
		SentAt: time.Now().UTC(), Payload: payload,
	}
	if _, err := control.Enqueue(t.Context(), diagnostics.SessionID, &spec, command); err != nil {
		t.Fatal(err)
	}
	var events []executioncontract.EventEnvelope
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err = control.ReplayEvents(t.Context(), spec.RunID, 0, 100)
		if err == nil && len(events) >= 4 && events[len(events)-1].Type == executioncontract.EventTerminal {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(events) < 4 {
		t.Fatalf("events = %+v, err=%v", events, err)
	}
	if events[0].Type != executioncontract.EventStarted || events[1].Type != executioncontract.EventOutput || events[len(events)-1].Type != executioncontract.EventTerminal {
		t.Fatalf("event order = %+v", events)
	}
	if !strings.Contains(string(events[2].Payload), "remote output") {
		t.Fatalf("fast assistant event was not delivered: %+v", events)
	}
	if err := executioncontract.ValidateEventOrder(events); err != nil {
		t.Fatal(err)
	}
	var handback workercontrol.ArtifactHandback
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handback, err = control.LoadStagedArtifact(t.Context(), spec.RunID)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || handback.Manifest.Workspace.BaseSHA != baseSHA || len(handback.Package.Members) == 0 {
		t.Fatalf("staged daemon handback = %+v, %v", handback, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(filepath.Join(cfg.WorkspaceRoot, spec.RunID)); errors.Is(statErr, os.ErrNotExist) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.WorkspaceRoot, spec.RunID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("acknowledged workspace was not cleaned up: %v", statErr)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("daemon exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := daemon.approvals.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolExhaustionIsExplicitAndPreservesPriorState(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 300)
	if err != nil {
		t.Fatal(err)
	}
	event := executioncontract.EventEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run",
		Sequence: 1, EventID: "event", IdempotencyKey: "key", Type: executioncontract.EventOutput,
		ObservedAt: time.Now().UTC(), Payload: json.RawMessage(`{"value":"` + strings.Repeat("x", 512) + `"}`),
	}
	if err := spool.appendEvent(event); !errors.Is(err, ErrSpoolExhausted) {
		t.Fatalf("append error = %v", err)
	}
	if got := spool.snapshot().RunSequences["run"]; got != 0 {
		t.Fatalf("failed append advanced sequence to %d", got)
	}
}

func TestRuntimeCapabilitiesReportBoundedSpoolUsage(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.appendEvent(executioncontract.EventEnvelope{RunID: "run", Sequence: 1, Type: executioncontract.EventOutput, Payload: json.RawMessage(`{"type":"assistant"}`)}); err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{cfg: Config{SpoolMaxBytes: 1 << 20}, spool: spool, capabilities: []string{"capacity=1"}}
	capabilities := daemon.runtimeCapabilities()
	joined := strings.Join(capabilities, " ")
	if !strings.Contains(joined, "spool_max_bytes=1048576") || strings.Contains(joined, "spool_bytes=0") {
		t.Fatalf("runtime capabilities = %v", capabilities)
	}
	payload, err := json.Marshal(spool.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(spool.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := spool.usageBytes(); got != int64(len(payload)) || info.Size() != got+1 {
		t.Fatalf("reported usage=%d payload=%d disk=%d; want payload units without formatting newline", got, len(payload), info.Size())
	}
}

func TestSpoolReservesRoomForTerminalFate(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 12288, 2)
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = spool.appendEvent(executioncontract.EventEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run",
			EventID: "output", Type: executioncontract.EventOutput, ObservedAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"value":"` + strings.Repeat("x", 512) + `"}`),
		})
		if errors.Is(err, ErrSpoolExhausted) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, runID := range []string{"run", "run-two"} {
		if err := spool.appendEvent(executioncontract.EventEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: runID,
			EventID: "terminal-" + runID, Type: executioncontract.EventTerminal, ObservedAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"state":"failed","error":"durable spool exhausted"}`),
		}); err != nil {
			t.Fatalf("terminal fate for %s did not fit capacity-aware reserve: %v", runID, err)
		}
	}
}

func TestAdmissionRejectionCannotSpendActiveRunTerminalReserve(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 8192, 1)
	if err != nil {
		t.Fatal(err)
	}
	for {
		err = spool.appendEvent(executioncontract.EventEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "active",
			Type: executioncontract.EventOutput, ObservedAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"value":"` + strings.Repeat("x", 256) + `"}`),
		})
		if errors.Is(err, ErrSpoolExhausted) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	rejection := executioncontract.EventEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "unadmitted",
		Type: executioncontract.EventTerminal, ObservedAt: time.Now().UTC(), Payload: json.RawMessage(`{"state":"failed"}`),
	}
	for i := 0; ; i++ {
		rejection.RunID = fmt.Sprintf("unadmitted-%d", i)
		err = spool.appendAdmissionEvent(rejection)
		if errors.Is(err, ErrSpoolExhausted) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	terminal := rejection
	terminal.RunID = "active"
	if err := spool.appendEvent(terminal); err != nil {
		t.Fatalf("active run terminal did not fit reserve: %v", err)
	}
}

func TestReplayMissingOutputPersistsRehydratedTail(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{logger: slog.New(slog.DiscardHandler), spool: spool}
	first := agent.StreamEvent{Type: "system", SessionID: "session", Timestamp: time.Now().UTC()}
	if err := daemon.emit("run", executioncontract.EventOutput, first); err != nil {
		t.Fatal(err)
	}
	recovered := &agent.Agent{}
	recovered.AppendOutput(first)
	recovered.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "while absent", Timestamp: time.Now().UTC()})
	recovered.AppendOutput(agent.StreamEvent{Type: "result", Content: "complete", Timestamp: time.Now().UTC()})
	if err := daemon.replayMissingOutput("run", recovered); err != nil {
		t.Fatal(err)
	}
	events := spool.snapshot().Events["run"]
	if len(events) != 3 || !strings.Contains(string(events[1].Payload), "while absent") ||
		!strings.Contains(string(events[2].Payload), `"type":"result"`) {
		t.Fatalf("replayed events = %+v", events)
	}
}

func TestDaemonReconcilesPersistedRunWithoutProviderProcess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	spool, err := OpenSpool(filepath.Join(root, "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.update(func(state *durableState) error {
		state.NodeID = "node-restart"
		state.RunAgents["run-lost"] = "agent-lost"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemon, err := New(ctx, Config{
		LeaderURL: "http://127.0.0.1:1", TokenEnv: "AGENTD_TEST_TOKEN", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	state := daemon.spool.snapshot()
	if len(state.RunAgents) != 0 {
		t.Fatalf("stale run mappings = %+v", state.RunAgents)
	}
	events := state.Events["run-lost"]
	if len(events) != 1 || events[0].Type != executioncontract.EventTerminal ||
		!strings.Contains(string(events[0].Payload), "unavailable after daemon restart") {
		t.Fatalf("reconciled events = %+v", events)
	}
}

func TestDaemonRestartReadoptsLiveProviderAndReportsItsTerminalFate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	repo, baseSHA := testRepository(t, root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' '{"type":"system","session_id":"restart-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"before restart"}]}}'
sleep 2
printf '%s\n' '{"type":"result","result":"after restart","session_id":"restart-session"}'
`
	if err := os.WriteFile(filepath.Join(bin, providerid.Claude), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	cfg := Config{
		LeaderURL: "http://127.0.0.1:1", TokenEnv: "AGENTD_TEST_TOKEN", NodeID: "node-restart", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20,
		Repositories: map[string]string{"repo": repo},
	}

	firstCtx, stopFirst := context.WithCancel(t.Context())
	first, err := New(firstCtx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	stubRunGrant(first)
	if err := first.handleCommand(firstCtx, workercontrol.Command{
		Sequence: 1, Envelope: commandForSpec(t, specWithBase("run-restart", baseSHA), "restart"),
	}); err != nil {
		t.Fatal(err)
	}
	waitForEventCount(t, first.spool, "run-restart", 2)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	if err := first.approvals.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	cancelShutdown()
	stopFirst()

	secondCtx, stopSecond := context.WithCancel(t.Context())
	defer stopSecond()
	second, err := New(secondCtx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if second.manager.RunningCount() != 1 {
		t.Fatalf("re-adopted running count = %d, want 1", second.manager.RunningCount())
	}
	if _, ok := second.agentForRun("run-restart"); !ok {
		t.Fatal("re-adopted provider is not addressable by protocol run id")
	}
	waitForTerminal(t, second.spool, "run-restart")
	if events := second.spool.snapshot().Events["run-restart"]; len(events) < 3 ||
		events[len(events)-1].Type != executioncontract.EventTerminal ||
		strings.Contains(string(events[len(events)-1].Payload), "unavailable after daemon restart") {
		t.Fatalf("restart events = %+v", events)
	}
}

func TestDaemonStartFailureBecomesAcknowledgableTerminalOutcome(t *testing.T) {
	root := t.TempDir()
	repo, baseSHA := testRepository(t, root)
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemon, err := New(ctx, Config{
		LeaderURL: "http://127.0.0.1:1", TokenEnv: "AGENTD_TEST_TOKEN", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20,
		Repositories: map[string]string{"repo": repo},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	stubRunGrant(daemon)
	spec := validSpec("run-start-failure")
	spec.Workspace.BaseSHA = baseSHA
	spec.Environment = []executioncontract.EnvironmentBinding{{
		Name: "SCOPED_INPUT", SecretRef: &executioncontract.SecretRef{Name: "run/run-start-failure/input"},
	}}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 1, Envelope: commandForSpec(t, spec, "start-failure")}); err != nil {
		t.Fatalf("durably reported start failure must be acknowledgable: %v", err)
	}
	events := daemon.spool.snapshot().Events[spec.RunID]
	if len(events) != 1 || events[0].Type != executioncontract.EventTerminal ||
		!strings.Contains(string(events[0].Payload), "unresolved secret capability") {
		t.Fatalf("start-failure events = %+v", events)
	}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 2, Envelope: commandForSpec(t, spec, "start-failure-replay")}); err != nil {
		t.Fatal(err)
	}
	if replayed := daemon.spool.snapshot().Events[spec.RunID]; len(replayed) != 1 {
		t.Fatalf("replayed start appended %d events, want one terminal", len(replayed))
	}
}

func TestProjectRunEnvironmentUsesProtectedFilesAndExcludesWorkerCredential(t *testing.T) {
	root := t.TempDir()
	layout := agentworkspace.Layout{
		RunRoot: filepath.Join(root, "run"), Worktree: filepath.Join(root, "run", "worktree"),
		Sidecar: filepath.Join(root, "run", "sidecar"), Artifact: filepath.Join(root, "run", "artifact"),
		WorkingMemory: filepath.Join(root, "run", "worktree"),
	}
	for _, dir := range []string{layout.Worktree, layout.Sidecar, layout.Artifact} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RUN_ONLY_SECRET", "scoped-value")
	t.Setenv("WORKER_MASTER_TOKEN", "worker-value")
	spec := validSpec("run-protected-secret")
	spec.Environment = []executioncontract.EnvironmentBinding{{
		Name: "SCOPED_INPUT", SecretRef: &executioncontract.SecretRef{Name: "run/run-protected-secret/input"},
	}}
	environment, err := projectRunEnvironment(layout, spec, map[string]string{"run/run-protected-secret/input": "RUN_ONLY_SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "scoped-value") || strings.Contains(joined, "worker-value") || strings.Contains(joined, "WORKER_MASTER_TOKEN") {
		t.Fatalf("provider environment leaked a credential: %s", joined)
	}
	prefix := "SCOPED_INPUT_FILE="
	var secretPath string
	for _, assignment := range environment {
		if value, ok := strings.CutPrefix(assignment, prefix); ok {
			secretPath = value
		}
	}
	data, err := os.ReadFile(secretPath)
	if err != nil || string(data) != "scoped-value" {
		t.Fatalf("protected secret = %q, %v", data, err)
	}
	info, err := os.Stat(secretPath)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("protected secret mode = %v, %v", info, err)
	}
	spec.Environment[0].Name = "../ESCAPE"
	if _, err := projectRunEnvironment(layout, spec, map[string]string{"run/run-protected-secret/input": "RUN_ONLY_SECRET"}); err == nil {
		t.Fatal("path-shaped secret binding name was accepted")
	}
}

func TestSpoolAssignsConcurrentPerRunSequencesAtomically(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const count = 64
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wg.Go(func() {
			errs <- spool.appendEvent(executioncontract.EventEnvelope{
				Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run",
				EventID: "event", Type: executioncontract.EventOutput, ObservedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`),
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events := spool.snapshot().Events["run"]
	sequences := make([]int, 0, len(events))
	for _, event := range events {
		sequences = append(sequences, int(event.Sequence))
	}
	sort.Ints(sequences)
	for i, got := range sequences {
		if want := i + 1; got != want {
			t.Fatalf("sequence[%d] = %d, want %d", i, got, want)
		}
	}
}

func TestDaemonForwardsLocalApprovalRequestAsProtocolProgress(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		logger: slog.New(slog.DiscardHandler), spool: spool,
		runAgents: map[string]string{"run": "agent"}, agentRuns: map[string]string{"agent": "run"},
		runCancels: make(map[string]context.CancelFunc),
	}
	daemon.emitManagerEvent(agentevents.AgentApproval("agent"), agent.ApprovalRequest{
		ToolUseID: "tool-1", ToolName: "Bash", Input: map[string]any{"command": "true"},
	})
	got := spool.snapshot().Events["run"]
	if len(got) != 1 || got[0].Type != executioncontract.EventProgress ||
		!strings.Contains(string(got[0].Payload), `"kind":"approval_request"`) ||
		!strings.Contains(string(got[0].Payload), `"toolUseId":"tool-1"`) {
		t.Fatalf("approval events = %+v", got)
	}
	if owner := spool.snapshot().PendingApprovals["tool-1"].RunID; owner != "run" {
		t.Fatalf("approval owner = %q, want run", owner)
	}
}

func TestSpoolRejectsCrossRunApprovalAndCompletesAtomically(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request := executioncontract.EventEnvelope{RunID: "run-a", Type: executioncontract.EventProgress}
	if err := spool.appendApprovalRequest(request, "tool-a", "fp-a"); err != nil {
		t.Fatal(err)
	}
	if err := spool.stageApproval("tool-a", durableApproval{RunID: "run-b", Approved: true, Fingerprint: "fp-a"}); err == nil {
		t.Fatal("cross-run approval must fail")
	}
	if err := spool.stageApproval("tool-a", durableApproval{RunID: "run-a", Approved: true, Fingerprint: "fp-a"}); err != nil {
		t.Fatal(err)
	}
	if err := spool.update(func(state *durableState) error {
		state.RunAgents["run-a"] = "agent-a"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	terminal := executioncontract.EventEnvelope{RunID: "run-a", Type: executioncontract.EventTerminal}
	if err := spool.appendTerminalAndComplete(terminal); err != nil {
		t.Fatal(err)
	}
	state := spool.snapshot()
	if _, ok := state.RunAgents["run-a"]; ok || state.PendingApprovals["tool-a"].RunID != "" {
		t.Fatalf("atomic completion retained ownership: %+v", state)
	}
	events := state.Events["run-a"]
	if len(events) != 2 || events[1].Type != executioncontract.EventTerminal {
		t.Fatalf("events = %+v", events)
	}
}

func TestDaemonTreatsStaleControlAsIdempotentNoOp(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		logger: slog.New(slog.DiscardHandler), spool: spool,
		runAgents: make(map[string]string), agentRuns: make(map[string]string), runCancels: make(map[string]context.CancelFunc),
	}
	for i, envelope := range []executioncontract.CommandEnvelope{
		{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "stale-steer", RunID: "gone",
			IdempotencyKey: "stale-steer", Type: executioncontract.CommandSteer, SentAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"text":"too late"}`),
		},
		{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "stale-approval", RunID: "gone",
			IdempotencyKey: "stale-approval", Type: executioncontract.CommandApprovalResponse, SentAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"toolUseId":"tool-gone","approved":true}`),
		},
	} {
		if err := daemon.handleCommand(t.Context(), workercontrol.Command{Sequence: uint64(i + 1), Envelope: envelope}); err != nil {
			t.Fatalf("stale %s = %v", envelope.Type, err)
		}
	}
}

func TestDaemonSteersStopsAndEnforcesLocalCapacity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	repo, baseSHA := testRepository(t, root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// A steerable fake consumes the initial stream-json prompt, announces it is
	// live, then consumes one steering message before completing.
	script := `#!/bin/sh
read -r initial
printf '%s\n' '{"type":"system","session_id":"steer-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"waiting"}]}}'
sleep 1
printf '%s\n' '{"type":"result","result":"first","session_id":"steer-session"}'
read -r steering
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"steered"}]}}'
printf '%s\n' '{"type":"result","result":"done","session_id":"steer-session"}'
`
	if err := os.WriteFile(filepath.Join(bin, providerid.Claude), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTD_TEST_TOKEN", "secret")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	daemon, err := New(ctx, Config{
		LeaderURL: "http://127.0.0.1:1", TokenEnv: "AGENTD_TEST_TOKEN", NodeID: "node-control", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report", WorkspaceRoot: filepath.Join(root, "workspaces"),
		StateRoot: filepath.Join(root, "state"), SpoolMaxBytes: 1 << 20,
		Repositories: map[string]string{"repo": repo},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	stubRunGrant(daemon)
	start := commandForSpec(t, specWithBase("run-steer", baseSHA), "start-steer")
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 1, Envelope: start}); err != nil {
		t.Fatal(err)
	}
	waitForEventCount(t, daemon.spool, "run-steer", 2)
	steerPayload, _ := json.Marshal(map[string]string{"text": "continue"})
	steer := executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "cmd-steer", RunID: "run-steer",
		IdempotencyKey: "steer-once", Type: executioncontract.CommandSteer, SentAt: time.Now().UTC(), Payload: steerPayload,
	}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 2, Envelope: steer}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 2, Envelope: steer}); err != nil {
		t.Fatal(err)
	}
	agentID, ok := daemon.agentForRun("run-steer")
	if !ok {
		t.Fatal("steered run is not active")
	}
	active, err := daemon.manager.GetAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if got := active.PendingPromptCount(); got != 1 {
		t.Fatalf("replayed steer queued %d prompts, want 1", got)
	}
	waitForTerminal(t, daemon.spool, "run-steer")
	waitForRunningCount(t, daemon, 0)

	// Replace the executable with a long-lived provider. The first run occupies
	// the only slot; a second start is rejected durably instead of spawning.
	hang := `#!/bin/sh
read -r initial
printf '%s\n' '{"type":"system","session_id":"hang-session"}'
sleep 30
`
	if err := os.WriteFile(filepath.Join(bin, providerid.Claude), []byte(hang), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 3, Envelope: commandForSpec(t, specWithBase("run-live", baseSHA), "start-live")}); err != nil {
		t.Fatal(err)
	}
	waitForEventCount(t, daemon.spool, "run-live", 2)
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 4, Envelope: commandForSpec(t, specWithBase("run-overflow", baseSHA), "start-overflow")}); err != nil {
		t.Fatal(err)
	}
	if daemon.manager.RunningCount() != 1 {
		t.Fatalf("running count = %d, want 1", daemon.manager.RunningCount())
	}
	overflow := daemon.spool.snapshot().Events["run-overflow"]
	if len(overflow) != 1 || overflow[0].Type != executioncontract.EventTerminal || !strings.Contains(string(overflow[0].Payload), "max concurrent") {
		t.Fatalf("overflow events = %+v", overflow)
	}
	stop := executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "cmd-stop", RunID: "run-live",
		IdempotencyKey: "stop-once", Type: executioncontract.CommandStop, SentAt: time.Now().UTC(),
	}
	if err := daemon.handleCommand(ctx, workercontrol.Command{Sequence: 5, Envelope: stop}); err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, daemon.spool, "run-live")
}

func commandForSpec(t *testing.T, spec executioncontract.RunSpec, commandID string) executioncontract.CommandEnvelope {
	t.Helper()
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	return executioncontract.CommandEnvelope{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: commandID, RunID: spec.RunID,
		IdempotencyKey: "command-" + commandID, Type: executioncontract.CommandStart, SentAt: time.Now().UTC(), Payload: payload,
	}
}

func stubRunGrant(daemon *Daemon) {
	daemon.issueGrant = func(context.Context, string, string) (workercontrol.RunGrant, error) {
		return workercontrol.RunGrant{Token: "test-scoped-run-grant", ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
}

func waitForEventCount(t *testing.T, spool *Spool, runID string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(spool.snapshot().Events[runID]) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %d events: %+v", runID, count, spool.snapshot().Events[runID])
}

func waitForTerminal(t *testing.T, spool *Spool, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events := spool.snapshot().Events[runID]
		if len(events) > 0 && events[len(events)-1].Type == executioncontract.EventTerminal {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not terminate: %+v", runID, spool.snapshot().Events[runID])
}

func waitForRunningCount(t *testing.T, daemon *Daemon, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.manager.RunningCount() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("running count = %d, want %d", daemon.manager.RunningCount(), want)
}

func validSpec(runID string) executioncontract.RunSpec {
	return executioncontract.RunSpec{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: runID,
		EffectID: "effect-" + runID, IdempotencyKey: "intent-" + runID,
		Fence: executioncontract.GenerationFence{TaskID: "task", TaskGeneration: 1, WorkflowID: "workflow", WorkflowGeneration: 1, StepID: "step"},
		Role:  "implementation", Provider: executioncontract.ProviderIntent{Provider: providerid.Claude, Model: "sonnet"},
		Prompt: executioncontract.Prompt{Text: "do the work"}, Deadline: time.Now().Add(time.Minute).UTC(),
		Workspace: executioncontract.Workspace{RepositoryID: "repo", BaseSHA: "0123456789012345678901234567890123456789", BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree}},
	}
}

func specWithBase(runID, baseSHA string) executioncontract.RunSpec {
	spec := validSpec(runID)
	spec.Workspace.BaseSHA = baseSHA
	return spec
}

func testRepository(t *testing.T, root string) (repoPath, baseSHA string) {
	t.Helper()
	repo := filepath.Join(root, "source")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}} {
		if err := gitexec.Run(ctx, gitexec.Options{Dir: repo}, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: repo}, "config", "commit.gpgsign", "false"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: repo}, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: repo}, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	sha, err := gitexec.Output(ctx, gitexec.Options{Dir: repo}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repo, sha
}
