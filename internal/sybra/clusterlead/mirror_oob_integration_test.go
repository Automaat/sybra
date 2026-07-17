package clusterlead

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/task"
)

// managerTaskService is a real, *task.Manager-backed adapter exposing the
// same three methods the production TaskService allowlists over the cluster
// HTTP API (ListTasks, GetTask, AssignTask) -- see internal/sybra/svc_tasks.go.
// Unlike the hand-rolled JSON stubs used elsewhere in this package's tests
// (followerStub in clusterlead_test.go), a call against this adapter flows
// through the real httpapi.Mount dispatcher into a real *task.Manager backed
// by real files under a temp dir, exercising the same code path an actual
// follower process runs.
type managerTaskService struct{ mgr *task.Manager }

func (s *managerTaskService) ListTasks() ([]task.Task, error) { return s.mgr.List() }

func (s *managerTaskService) GetTask(id string) (task.Task, error) { return s.mgr.Get(id) }

func (s *managerTaskService) AssignTask(t task.Task) error {
	_, _, err := s.mgr.Put(t)
	return err
}

// realFollowerServer stands up a genuine HTTP server, mounted with
// httpapi.Mount the way a real follower process wires its TaskService,
// backed by a real task.Manager/task.Store rooted at a temp dir.
func realFollowerServer(t *testing.T, mgr *task.Manager) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(&managerTaskService{mgr: mgr}, "ListTasks", "GetTask", "AssignTask"),
	}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMirrorPropagatesOutOfBandFollowerWrite is an independent, from-scratch
// two-process-style repro of the exact scenario that motivated #2188: an
// external write applied straight to a follower's task.Store, bypassing that
// follower's own httpapi entirely (the way `sybra-cli` run over plain SSH
// with no SYBRA_PORT/SYBRA_AUTH_TOKEN reaches a follower host). It uses a
// real httpapi-mounted HTTP server backed by a real task.Manager on both
// sides, and drives sync purely through mirror.Run's real ticker -- not by
// calling reconcileNode/applyFollowerTask directly.
func TestMirrorPropagatesOutOfBandFollowerWrite(t *testing.T) {
	followerMgr := newManager(t)
	srv := realFollowerServer(t, followerMgr)

	leaderMgr := newManager(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}

	t0 := time.Now().Add(-time.Hour)
	// Leader's canonical copy, as it would look right after Assigner pushed
	// the task to the follower and stamped AssignedNode.
	if _, _, err := leaderMgr.Put(task.Task{
		ID: "task-oob", Title: "t", Status: task.StatusInProgress,
		ProjectID: "owner/pet", AssignedNode: "pet-box", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	// The follower's own copy of the same task, as AssignTask would have
	// written it there.
	if _, _, err := followerMgr.Put(task.Task{
		ID: "task-oob", Title: "t", Status: task.StatusInProgress,
		ProjectID: "owner/pet", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, leaderMgr, roster, slog.New(slog.DiscardHandler), 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); mirror.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// Give the mirror a head start, then perform the out-of-band write
	// straight against the follower's task.Store -- never touching the
	// follower's httpapi mux at all.
	time.Sleep(30 * time.Millisecond)
	if _, err := followerMgr.Update("task-oob", task.Update{
		Status: task.Ptr(task.StatusDone),
		Branch: task.Ptr("feat/oob-write"),
	}); err != nil {
		t.Fatalf("out-of-band follower write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var got task.Task
	for time.Now().Before(deadline) {
		got, err = leaderMgr.Get("task-oob")
		if err == nil && got.Status == task.StatusDone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got.Status != task.StatusDone {
		t.Fatalf("leader canonical status = %q, want done -- an out-of-band write on the follower's store never propagated to the leader via polling", got.Status)
	}
	if got.Branch != "feat/oob-write" {
		t.Errorf("leader canonical branch = %q, want feat/oob-write", got.Branch)
	}
}

// TestMirrorReconcileSucceedsPastOldThirtyTwoMegabyteCap attacks the size-cap
// fix with a real oversized payload transmitted over a real HTTP connection
// (chunked transfer, real read loop) -- not the author's own tiny
// shrunk-cap-to-100-bytes unit test. A ~40MB real ListTasks response exceeds
// the OLD 32MB internal/cluster.maxResponseBody while staying well under the
// new 256MB one, so success here specifically demonstrates the cap raise (not
// merely that the truncation-detection logic compiles).
func TestMirrorReconcileSucceedsPastOldThirtyTwoMegabyteCap(t *testing.T) {
	followerMgr := newManager(t)
	srv := realFollowerServer(t, followerMgr)

	leaderMgr := newManager(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}

	t0 := time.Now().Add(-time.Hour)
	if _, _, err := leaderMgr.Put(task.Task{
		ID: "task-big", Title: "t", Status: task.StatusInProgress,
		ProjectID: "owner/pet", AssignedNode: "pet-box", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}

	bigBody := strings.Repeat("x", 40<<20) // 40MB of real bytes, transmitted over real loopback HTTP
	if _, _, err := followerMgr.Put(task.Task{
		ID: "task-big", Title: "t", Status: task.StatusDone,
		ProjectID: "owner/pet", Body: bigBody, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, leaderMgr, roster, slog.New(slog.DiscardHandler), 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); mirror.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(10 * time.Second)
	var got task.Task
	var gerr error
	for time.Now().Before(deadline) {
		got, gerr = leaderMgr.Get("task-big")
		if gerr == nil && got.Status == task.StatusDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got.Status != task.StatusDone {
		t.Fatalf("leader never applied the >32MB follower payload (status=%q, err=%v) -- would have silently failed under the pre-#2188 32MB cap", got.Status, gerr)
	}
	if len(got.Body) != len(bigBody) {
		t.Errorf("mirrored Body length = %d, want %d -- payload may have been truncated in transit", len(got.Body), len(bigBody))
	}
}

// TestMirrorReconcileLoopEscalatesOnUnreachableFollowerOverRealTicks attacks
// the Warn-then-Error escalation for a real failure mode -- nothing
// listening on the follower's port -- driven by the actual
// time.Ticker-based reconcileLoop via mirror.Run over many real ticks, not
// by calling reconcileNode directly in a test-authored loop (the author's
// own TestMirrorReconcileEscalatesLogLevelOnRepeatedFailure does the latter,
// against a stubbed 401).
func TestMirrorReconcileLoopEscalatesOnUnreachableFollowerOverRealTicks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadEndpoint := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := leaderConfig(deadEndpoint, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mirror := NewMirror(cfg, mgr, roster, logger, 15*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	mirror.Run(ctx) // blocks until ctx's timeout -- real ticker, real elapsed time

	out := buf.String()
	warnCount := strings.Count(out, "level=WARN")
	errCount := strings.Count(out, "level=ERROR")
	if warnCount < reconcileFailureEscalateThreshold-1 {
		t.Errorf("warnCount = %d, want at least %d Warn-level reconcile failure logs before escalation, got log:\n%s", warnCount, reconcileFailureEscalateThreshold-1, out)
	}
	if errCount == 0 {
		t.Errorf("no Error-level reconcile failure logged after %d+ consecutive real-tick failures against an unreachable follower, got log:\n%s", reconcileFailureEscalateThreshold, out)
	}
}

// TestMirrorRunHandlesSlowAndFastFollowersConcurrentlyAndCancelsCleanly
// drives two followers on independent tickers through one Mirror.Run call --
// one that responds immediately every tick, one whose handler blocks
// mid-request -- then cancels ctx while the slow follower's request is still
// in flight. It asserts Run returns promptly (the cancelled context actually
// aborts the in-flight HTTP call rather than leaking a goroutine/hanging)
// and that the fast follower kept reconciling concurrently the whole time.
// Run under -race to also catch data races between the two reconcileLoop
// goroutines sharing one Mirror.
func TestMirrorRunHandlesSlowAndFastFollowersConcurrentlyAndCancelsCleanly(t *testing.T) {
	var fastHits atomic.Int32
	fastMux := http.NewServeMux()
	fastMux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	fastMux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/ListTasks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fastHits.Add(1)
		_, _ = io.WriteString(w, "[]")
	})
	fastSrv := httptest.NewServer(fastMux)
	t.Cleanup(fastSrv.Close)

	blockedReq := make(chan struct{})
	var blockedOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }

	slowMux := http.NewServeMux()
	slowMux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	slowMux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/ListTasks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		blockedOnce.Do(func() { close(blockedReq) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "[]")
	})
	slowSrv := httptest.NewServer(slowMux)
	t.Cleanup(func() {
		closeRelease()
		slowSrv.Close()
	})

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{
			{Name: "fast-box", Endpoints: []string{fastSrv.URL}},
			{Name: "slow-box", Endpoints: []string{slowSrv.URL}},
		},
	}}
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); mirror.Run(ctx) }()

	select {
	case <-blockedReq:
	case <-time.After(2 * time.Second):
		closeRelease()
		cancel()
		<-done
		t.Fatal("slow follower's handler was never hit")
	}

	// Give the fast follower's independent ticker goroutine a chance to land
	// at least one tick concurrently with the slow follower's still-blocked
	// request before cancelling -- otherwise this races the two goroutines'
	// first synchronous reconcileNode call and can flake on scheduling
	// alone, independent of anything under test.
	fastDeadline := time.Now().Add(2 * time.Second)
	for fastHits.Load() == 0 && time.Now().Before(fastDeadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fastHits.Load() == 0 {
		closeRelease()
		cancel()
		<-done
		t.Fatal("fast follower's reconcile loop never ran concurrently with the slow one")
	}

	// Cancel while the slow follower's ListTasks call is still in flight
	// (its handler is parked on <-release, not yet returned).
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		closeRelease()
		t.Fatal("Mirror.Run did not return within 2s of ctx cancellation -- the slow follower's in-flight request outlived the cancelled context (goroutine leak / hang)")
	}
	closeRelease()

	if fastHits.Load() == 0 {
		t.Error("fast follower's reconcile loop never ran concurrently with the slow one")
	}
}
