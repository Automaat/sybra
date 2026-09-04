package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// This is the process boundary used in production (real HTTP, durable leader
// database, two independently registered daemons and real provider subprocesses).
// The selective transport fault models a partition after command delivery: the
// paid provider must finish once, spool locally, and replay in order when the
// link returns rather than causing a replacement run.
func TestTwoDaemonsPartitionReplaysWithoutReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	repository, baseSHA := testRepository(t, root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(bin, providerid.Claude)
	if err := os.WriteFile(provider, []byte(`#!/bin/sh
printf '%s\n' '{"type":"system","session_id":"partition-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"durable partition output"}]}}'
printf '%s\n' '{"type":"result","result":"done","session_id":"partition-session","total_cost_usd":0.01}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTD_PARTITION_TOKEN", "test-token")

	control := workercontrol.New(dbtest.SQLite(t))
	server := httptest.NewServer(control.Handler())
	t.Cleanup(server.Close)
	newDaemon := func(node string) *Daemon {
		cfg := Config{
			LeaderURL: server.URL, TokenEnv: "AGENTD_PARTITION_TOKEN", NodeID: node, Capacity: 1,
			Providers: []string{providerid.Claude}, SandboxMode: "report", LeaseSeconds: 30, PollSeconds: 1,
			WorkspaceRoot: filepath.Join(root, node, "workspaces"), StateRoot: filepath.Join(root, node, "state"),
			SpoolMaxBytes: 1 << 20, Repositories: map[string]string{"repo": repository},
		}
		daemon, err := New(t.Context(), cfg, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		return daemon
	}
	first, second := newDaemon("node-partitioned"), newDaemon("node-healthy")
	var partitioned atomic.Bool
	baseTransport := http.DefaultTransport
	first.client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if partitioned.Load() && (request.URL.Path == "/worker/v1/events" || request.URL.Path == "/worker/v1/artifacts") {
			return nil, errors.New("synthetic partition")
		}
		return baseTransport.RoundTrip(request)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- first.Run(ctx) }()
	go func() { done <- second.Run(ctx) }()
	waitForWorkers(t, control, 2)

	partitioned.Store(true)
	schedule := func(runID, node string) workercontrol.Placement {
		spec := validSpec(runID)
		spec.EffectID = "effect-" + runID
		spec.Workspace.BaseSHA = baseSHA
		payload, _ := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
		command := executioncontract.CommandEnvelope{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", CommandID: "start-" + runID,
			RunID: runID, IdempotencyKey: "start:" + runID, Type: executioncontract.CommandStart,
			SentAt: time.Now().UTC(), Payload: payload,
		}
		placed, err := control.ScheduleStart(t.Context(), workercontrol.PlacementRequest{
			Spec: spec, Command: command, NodeOverride: node, AllowLocalFallback: false, Sandbox: "report",
		})
		if err != nil {
			t.Fatal(err)
		}
		return placed
	}
	firstPlacement := schedule("run-partitioned", "node-partitioned")
	schedule("run-healthy", "node-healthy")
	waitForTerminal(t, first.spool, "run-partitioned")
	waitForLeaderTerminal(t, control, "run-healthy")
	if events, _ := control.ReplayEvents(t.Context(), "run-partitioned", 0, 100); len(events) != 0 {
		t.Fatalf("partitioned events reached leader early: %+v", events)
	}

	partitioned.Store(false)
	events := waitForLeaderTerminal(t, control, "run-partitioned")
	if err := executioncontract.ValidateEventOrder(events); err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for i := range events {
		if events[i].Type == executioncontract.EventTerminal {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal count = %d, events=%+v", terminals, events)
	}
	status, err := control.RemoteRunStatus(t.Context(), "run-partitioned")
	if err != nil || status.SessionID != firstPlacement.SessionID {
		t.Fatalf("partition caused reassignment: status=%+v err=%v", status, err)
	}

	cancel()
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("daemon exit = %v", err)
			}
		case <-time.After(scaledDeadline(5 * time.Second)):
			t.Fatal("daemon did not stop")
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), scaledDeadline(5*time.Second))
	defer shutdownCancel()
	_ = first.approvals.Shutdown(shutdownCtx)
	_ = second.approvals.Shutdown(shutdownCtx)
}

func waitForWorkers(t *testing.T, control *workercontrol.Service, count int) {
	t.Helper()
	deadline := time.Now().Add(scaledDeadline(5 * time.Second))
	for time.Now().Before(deadline) {
		if diagnostics, err := control.Diagnostics(t.Context()); err == nil && len(diagnostics) == count {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker count did not reach %d", count)
}

func waitForLeaderTerminal(t *testing.T, control *workercontrol.Service, runID string) []executioncontract.EventEnvelope {
	t.Helper()
	deadline := time.Now().Add(scaledDeadline(10 * time.Second))
	for time.Now().Before(deadline) {
		events, err := control.ReplayEvents(t.Context(), runID, 0, 100)
		if err == nil && len(events) > 0 && events[len(events)-1].Type == executioncontract.EventTerminal {
			return events
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("leader did not receive terminal for %s", runID)
	return nil
}
