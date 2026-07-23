package clusterlead

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
)

type followerStub struct {
	mu       sync.Mutex
	assigned []task.Task
	tasks    []task.Task
	// live overrides GetTask, letting a test make it disagree with tasks
	// (the ListTasks snapshot) to simulate a follower that moved on.
	live map[string]task.Task
}

func (f *followerStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/TaskService/AssignTask":
			var args []task.Task
			_ = json.Unmarshal(body, &args)
			if len(args) == 1 {
				f.mu.Lock()
				f.assigned = append(f.assigned, args[0])
				f.mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		case "/api/TaskService/ListTasks":
			f.mu.Lock()
			_ = json.NewEncoder(w).Encode(f.tasks)
			f.mu.Unlock()
		case "/api/TaskService/GetTask":
			var args []string
			_ = json.Unmarshal(body, &args)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(args) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if liveTask, ok := f.live[args[0]]; ok {
				_ = json.NewEncoder(w).Encode(liveTask)
				return
			}
			for i := range f.tasks {
				if f.tasks[i].ID == args[0] {
					_ = json.NewEncoder(w).Encode(f.tasks[i])
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *followerStub) lastAssigned() (task.Task, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.assigned) == 0 {
		return task.Task{}, false
	}
	return f.assigned[len(f.assigned)-1], true
}

func leaderConfig(followerURL string, homes []string) *config.Config {
	return &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "pet-box",
			Endpoints: []string{followerURL},
			Homes:     homes,
		}},
	}}
}

func newManager(t *testing.T) *task.Manager {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return task.NewManager(store, task.EmitterFunc(func(string, any) {}))
}

func TestAssignerRoutesRemoteAndStamps(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, func(string) bool { return false }, nil, nil)

	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Title: "t", Status: task.StatusTodo, ProjectID: "owner/pet"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := mgr.Get("task-pet")

	routed, err := assigner.Route(context.Background(), cur)
	if err != nil || !routed {
		t.Fatalf("Route remote: routed=%v err=%v", routed, err)
	}
	got, ok := stub.lastAssigned()
	if !ok || got.ID != "task-pet" || got.AssignedNode != "pet-box" {
		t.Fatalf("follower did not receive the assigned task with node stamp: %+v", got)
	}
	after, _ := mgr.Get("task-pet")
	if after.AssignedNode != "pet-box" {
		t.Errorf("canonical AssignedNode = %q, want pet-box", after.AssignedNode)
	}
}

func TestAssignerRouteIsIdempotentOnceStamped(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, func(string) bool { return false }, nil, nil)

	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Title: "t", Status: task.StatusTodo, ProjectID: "owner/pet"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := mgr.Get("task-pet")
	if routed, err := assigner.Route(context.Background(), cur); err != nil || !routed {
		t.Fatalf("first Route: routed=%v err=%v", routed, err)
	}

	stamped, _ := mgr.Get("task-pet")
	if routed, err := assigner.Route(context.Background(), stamped); err != nil {
		t.Fatalf("second Route: %v", err)
	} else if routed {
		t.Fatal("second Route should no-op once AssignedNode already matches home")
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.assigned) != 1 {
		t.Fatalf("Route pushed %d times, want 1", len(stub.assigned))
	}
}

func TestAssignerPushUpdateReSyncsAlreadyStampedTask(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, func(string) bool { return false }, nil, nil)

	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Title: "t", Status: task.StatusTodo, ProjectID: "owner/pet"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := mgr.Get("task-pet")
	if routed, err := assigner.Route(context.Background(), cur); err != nil || !routed {
		t.Fatalf("first Route: routed=%v err=%v", routed, err)
	}

	// Simulate a leader-side edit after the initial push (e.g. the umbrella
	// gate clearing a dependency block) — Route alone would never re-push
	// this since AssignedNode already matches home.
	stamped, _ := mgr.Get("task-pet")
	stamped.StatusReason = "umbrella dependencies satisfied"
	edited, _, err := mgr.Put(stamped)
	if err != nil {
		t.Fatal(err)
	}

	pushed, err := assigner.PushUpdate(context.Background(), edited)
	if err != nil || !pushed {
		t.Fatalf("PushUpdate: pushed=%v err=%v", pushed, err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.assigned) != 2 {
		t.Fatalf("follower received %d pushes, want 2 (initial Route + PushUpdate)", len(stub.assigned))
	}
	if got := stub.assigned[1]; got.StatusReason != "umbrella dependencies satisfied" {
		t.Errorf("second push StatusReason = %q, want the edited value", got.StatusReason)
	}
}

func TestAssignerLeavesLocalTaskAlone(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, func(string) bool { return false }, nil, nil)

	local := task.Task{ID: "task-local", Title: "t", Status: task.StatusTodo, ProjectID: "owner/other"}
	routed, err := assigner.Route(context.Background(), local)
	if err != nil {
		t.Fatal(err)
	}
	if routed {
		t.Error("a locally-homed task must not be routed to a follower")
	}
	if _, ok := stub.lastAssigned(); ok {
		t.Error("follower received a task it should not have")
	}
}

func TestAssignerTickIsIdempotent(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, func(string) bool { return false }, nil, nil)

	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Title: "t", Status: task.StatusTodo, ProjectID: "owner/pet"}); err != nil {
		t.Fatal(err)
	}
	assigner.Tick(context.Background())
	assigner.Tick(context.Background())

	stub.mu.Lock()
	n := len(stub.assigned)
	stub.mu.Unlock()
	if n != 1 {
		t.Errorf("Tick pushed the task %d times, want 1 (idempotent after AssignedNode stamped)", n)
	}
}

func TestMirrorApplyConvergesAndDropsStale(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{
		ID: "task-pet", Title: "leader title", Status: task.StatusTodo,
		ProjectID: "owner/pet", AssignedNode: "pet-box", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}

	newer := task.Task{
		ID: "task-pet", Title: "follower title", Status: task.StatusInReview,
		ProjectID: "attacker/x", AssignedNode: "elsewhere", Branch: "feat/x", UpdatedAt: t0.Add(time.Hour),
	}
	if !mirror.applyFollowerTask("pet-box", newer) {
		t.Fatal("newer follower state must apply")
	}
	got, _ := mgr.Get("task-pet")
	if got.Status != task.StatusInReview || got.Branch != "feat/x" {
		t.Errorf("execution not mirrored: %+v", got)
	}
	if got.Title != "leader title" || got.ProjectID != "owner/pet" || got.AssignedNode != "pet-box" {
		t.Errorf("identity was overwritten by follower: %+v", got)
	}
	if got.MirrorRev != 1 {
		t.Errorf("MirrorRev = %d, want 1", got.MirrorRev)
	}

	stale := newer
	stale.Status = task.StatusTodo
	stale.UpdatedAt = t0.Add(30 * time.Minute)
	if mirror.applyFollowerTask("pet-box", stale) {
		t.Error("stale (older UpdatedAt) follower state must be dropped")
	}
	got, _ = mgr.Get("task-pet")
	if got.Status != task.StatusInReview || got.MirrorRev != 1 {
		t.Errorf("stale update mutated canonical: %+v", got)
	}
}

func TestMirrorClearsSidecarWhenFollowerClears(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Status: task.StatusTodo, AssignedNode: "pet-box", UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}

	withPlan := task.Task{ID: "task-pet", Status: task.StatusPlanning, AssignedNode: "pet-box", Plan: "the plan", UpdatedAt: t0.Add(time.Hour)}
	if !mirror.applyFollowerTask("pet-box", withPlan) {
		t.Fatal("apply with plan")
	}
	if got, _ := mgr.Get("task-pet"); got.Plan != "the plan" {
		t.Fatalf("plan not mirrored: %q", got.Plan)
	}

	cleared := task.Task{ID: "task-pet", Status: task.StatusInProgress, AssignedNode: "pet-box", Plan: "", UpdatedAt: t0.Add(2 * time.Hour)}
	if !mirror.applyFollowerTask("pet-box", cleared) {
		t.Fatal("apply with cleared plan")
	}
	if got, _ := mgr.Get("task-pet"); got.Plan != "" {
		t.Errorf("follower cleared its plan but leader kept stale sidecar: %q", got.Plan)
	}
}

func TestMirrorMirrorsPlanningSidecars(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{ID: "task-pet", Status: task.StatusTodo, AssignedNode: "pet-box", UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}

	follower := task.Task{
		ID:            "task-pet",
		Status:        task.StatusPlanReview,
		AssignedNode:  "pet-box",
		Plan:          "the plan",
		PlanContract:  `{"task_id":"task-pet"}`,
		PlanCritique:  "the critique",
		PlanResearch:  "the research",
		PlanDecisions: "# Decisions\n\nNo open decisions.",
		PlanBrief:     "the brief",
		CodeReview:    "the review",
		UpdatedAt:     t0.Add(time.Hour),
	}
	if !mirror.applyFollowerTask("pet-box", follower) {
		t.Fatal("apply follower sidecars")
	}

	got, err := mgr.Get("task-pet")
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != follower.Plan ||
		got.PlanContract != follower.PlanContract ||
		got.PlanCritique != follower.PlanCritique ||
		got.PlanResearch != follower.PlanResearch ||
		got.PlanDecisions != follower.PlanDecisions ||
		got.PlanBrief != follower.PlanBrief ||
		got.CodeReview != follower.CodeReview {
		t.Fatalf("mirrored sidecars mismatch:\n got: %+v\nwant: %+v", got, follower)
	}

	cleared := task.Task{
		ID:           "task-pet",
		Status:       task.StatusInProgress,
		AssignedNode: "pet-box",
		UpdatedAt:    t0.Add(2 * time.Hour),
	}
	if !mirror.applyFollowerTask("pet-box", cleared) {
		t.Fatal("apply cleared follower sidecars")
	}

	got, err = mgr.Get("task-pet")
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "" ||
		got.PlanContract != "" ||
		got.PlanCritique != "" ||
		got.PlanResearch != "" ||
		got.PlanDecisions != "" ||
		got.PlanBrief != "" ||
		got.CodeReview != "" {
		t.Fatalf("cleared follower sidecars should clear leader sidecars: %+v", got)
	}
}

// fakeAnomalySink is a monitor.IssueSink test double that records every
// submitted anomaly so tests can assert alerting fired without depending on
// GitHub/local-task-routing machinery.
type fakeAnomalySink struct {
	mu    sync.Mutex
	calls []monitor.Anomaly
}

func (s *fakeAnomalySink) Submit(_ context.Context, a monitor.Anomaly, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, a)
	return true, nil
}

func (s *fakeAnomalySink) submitted() []monitor.Anomaly {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// TestMirrorDetectsAndRepairsTagsAndDependsOnDrift covers issue #2350: Tags
// and DependsOn are leader-authoritative fields Merge never pulls from the
// follower (see Merge's field list — only execution fields like Status flow
// follower-authoritative). If a leader-side write to either one never
// reached the follower, nothing in the ordinary reconcile loop would ever
// notice. This seeds a follower report that disagrees with the canonical
// copy on both fields and asserts the sweep detects it, alerts through the
// anomaly sink, and repairs the follower within the same reconcile pass —
// without touching the follower's own Status/PR fields (never a stale
// full-task overwrite).
func TestMirrorDetectsAndRepairsTagsAndDependsOnDrift(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)
	sink := &fakeAnomalySink{}
	mirror.SetAnomalySink(sink)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	canonical := task.Task{
		ID:           "task-pet",
		Status:       task.StatusTodo,
		AssignedNode: "pet-box",
		Tags:         []string{"backend", "umbrella-gated"},
		DependsOn:    []string{"https://github.com/o/r/issues/1"},
		UpdatedAt:    t0,
	}
	if _, _, err := mgr.Put(canonical); err != nil {
		t.Fatal(err)
	}

	// The follower reports real progress (a status advance the leader hasn't
	// pulled yet — expected and fine) but still carries the stale Tags/
	// DependsOn from before the leader's edit never reached it.
	stale := task.Task{
		ID:           "task-pet",
		Status:       task.StatusInProgress,
		AssignedNode: "pet-box",
		Tags:         []string{"backend"},
		DependsOn:    nil,
		UpdatedAt:    t0.Add(time.Hour),
	}
	// repairDrift re-fetches via GetTask rather than trusting this tick's
	// ListTasks snapshot; seed both to the same value here since this test
	// covers ordinary (non-racing) repair.
	stub.tasks = []task.Task{stale}
	if !mirror.applyFollowerTask("pet-box", stale) {
		t.Fatal("apply follower report")
	}

	// Detected + alerted.
	calls := sink.submitted()
	if len(calls) != 1 {
		t.Fatalf("anomaly sink got %d submissions, want 1: %+v", len(calls), calls)
	}
	if calls[0].Kind != monitor.KindClusterDrift || calls[0].TaskID != "task-pet" {
		t.Fatalf("submitted anomaly = %+v, want KindClusterDrift for task-pet", calls[0])
	}

	// Repaired: the follower stub received an AssignTask carrying the
	// canonical Tags/DependsOn...
	got, ok := stub.lastAssigned()
	if !ok {
		t.Fatal("follower did not receive a repair push")
	}
	if !slices.Equal(got.Tags, canonical.Tags) {
		t.Errorf("repaired tags = %v, want %v", got.Tags, canonical.Tags)
	}
	if !slices.Equal(got.DependsOn, canonical.DependsOn) {
		t.Errorf("repaired depends_on = %v, want %v", got.DependsOn, canonical.DependsOn)
	}
	// ...but the follower's own execution state (Status) was left as its
	// current report, not rolled back to the leader's stale canonical copy.
	if got.Status != task.StatusInProgress {
		t.Errorf("repair push overwrote follower status: got %q, want %q (must not roll back execution state)", got.Status, task.StatusInProgress)
	}

	// Applying normally still landed the follower's Status on the leader —
	// drift detection doesn't block the ordinary merge.
	if leaderCopy, err := mgr.Get("task-pet"); err != nil || leaderCopy.Status != task.StatusInProgress {
		t.Errorf("leader canonical status = %+v, err=%v, want in-progress merged normally", leaderCopy, err)
	}
}

// TestMirrorDriftRepairUsesLiveFollowerStateNotStaleSnapshot covers an
// adversarial-review finding: this reconcile tick's ListTasks response (the
// `follower` value Merge/detectAndRepairDrift work from) can already be
// behind the follower's actual current state by the time repairDrift's
// AssignTask lands — earlier tasks in the same batch, under the same
// applyMu lock, each take time first. Patching that stale snapshot's
// Tags/DependsOn and pushing it verbatim would silently roll back whatever
// the follower did since. The follower stub here answers ListTasks and
// GetTask differently — GetTask (live, queried at repair time) reports a
// status advance and a fresh AgentRuns entry ListTasks (the snapshot) never
// saw — and asserts the repair preserves the live state, not the snapshot.
func TestMirrorDriftRepairUsesLiveFollowerStateNotStaleSnapshot(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	canonical := task.Task{
		ID:           "task-pet",
		Status:       task.StatusTodo,
		AssignedNode: "pet-box",
		Tags:         []string{"backend", "umbrella-gated"},
		UpdatedAt:    t0,
	}
	if _, _, err := mgr.Put(canonical); err != nil {
		t.Fatal(err)
	}

	snapshot := task.Task{
		ID:           "task-pet",
		Status:       task.StatusTodo,
		AssignedNode: "pet-box",
		Tags:         []string{"backend"},
		UpdatedAt:    t0.Add(time.Hour),
	}
	moved := task.Task{
		ID:           "task-pet",
		Status:       task.StatusInProgress,
		AssignedNode: "pet-box",
		Tags:         []string{"backend"},
		AgentRuns:    []task.AgentRun{{AgentID: "started-after-the-snapshot"}},
		UpdatedAt:    t0.Add(2 * time.Hour),
	}
	stub.tasks = []task.Task{snapshot}
	stub.live = map[string]task.Task{"task-pet": moved}

	if !mirror.applyFollowerTask("pet-box", snapshot) {
		t.Fatal("apply follower report")
	}

	got, ok := stub.lastAssigned()
	if !ok {
		t.Fatal("follower did not receive a repair push")
	}
	if !slices.Equal(got.Tags, canonical.Tags) {
		t.Errorf("repaired tags = %v, want %v", got.Tags, canonical.Tags)
	}
	if got.Status != moved.Status {
		t.Errorf("repair overwrote live status %q with the stale snapshot's %q — rolled back follower progress", moved.Status, got.Status)
	}
	if len(got.AgentRuns) != 1 || got.AgentRuns[0].AgentID != "started-after-the-snapshot" {
		t.Errorf("repair dropped the follower's live AgentRuns, got %+v — rolled back follower progress", got.AgentRuns)
	}
}

// TestMirrorNoAlertOnOrdinaryStatusDisagreement asserts that Status differing
// between the leader and follower — the normal, expected, self-healing case
// Merge exists for — never fires the drift alert/repair path. Only
// Tags/DependsOn (fields Merge doesn't carry) are drift-worthy; alerting on
// every ordinary status lag would make the signal useless.
func TestMirrorNoAlertOnOrdinaryStatusDisagreement(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)
	sink := &fakeAnomalySink{}
	mirror.SetAnomalySink(sink)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{
		ID: "task-pet", Status: task.StatusTodo, AssignedNode: "pet-box",
		Tags: []string{"backend"}, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}

	advanced := task.Task{
		ID: "task-pet", Status: task.StatusPlanReview, AssignedNode: "pet-box",
		Tags: []string{"backend"}, UpdatedAt: t0.Add(time.Hour),
	}
	if !mirror.applyFollowerTask("pet-box", advanced) {
		t.Fatal("apply follower report")
	}

	if calls := sink.submitted(); len(calls) != 0 {
		t.Fatalf("anomaly sink got %d submissions for an ordinary status advance, want 0: %+v", len(calls), calls)
	}
	if _, ok := stub.lastAssigned(); ok {
		t.Error("follower received an unnecessary repair push for a matching Tags/DependsOn task")
	}
}

func TestMirrorIgnoresTaskOwnedByAnotherNode(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if _, _, err := mgr.Put(task.Task{ID: "task-x", Status: task.StatusTodo, AssignedNode: "other-node", UpdatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	follower := task.Task{ID: "task-x", Status: task.StatusDone, UpdatedAt: time.Unix(100, 0)}
	if mirror.applyFollowerTask("pet-box", follower) {
		t.Error("a task the leader already has assigned to a different node must not be mirrored from this one")
	}
}

// TestMirrorAdoptsUnknownFollowerTask covers the mirror gap behind
// c01cafd6's local-board investigation: a task with no prior canonical copy
// (the leader has never heard of it) is not necessarily a stray to ignore —
// it may be a follower's own self-originated work (umbrella expansion or
// triage run directly on that node, with no leader routing involved, so
// never assigned a node). Since ListTasksForNode only ever returns tasks
// that genuinely live on the node being asked, the leader can safely adopt
// any task it sees there for the first time.
func TestMirrorAdoptsUnknownFollowerTask(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	follower := task.Task{
		ID: "self-originated", Title: "made on pet-box", ProjectID: "owner/pet",
		Status: task.StatusTodo, UpdatedAt: time.Unix(100, 0),
	}
	if !mirror.applyFollowerTask("pet-box", follower) {
		t.Fatal("expected the leader to adopt a task it has never seen before")
	}
	got, err := mgr.Get("self-originated")
	if err != nil {
		t.Fatalf("adopted task not found in canonical store: %v", err)
	}
	if got.AssignedNode != "pet-box" {
		t.Errorf("AssignedNode = %q, want pet-box", got.AssignedNode)
	}
	if got.Title != "made on pet-box" || got.ProjectID != "owner/pet" {
		t.Errorf("adopted task lost identity fields: %+v", got)
	}
	if got.MirrorRev != 1 {
		t.Errorf("MirrorRev = %d, want 1 on first adoption", got.MirrorRev)
	}
}

// TestMirrorRunPollsOnlyNeverSubscribes covers #2188: Run must sync purely by
// polling ListTasks and never attempt an SSE /events subscription — the SSE
// path never fired for filesystem-direct follower writes and is no longer
// part of Mirror at all.
func TestMirrorRunPollsOnlyNeverSubscribes(t *testing.T) {
	var eventsHit atomic.Bool
	var mu sync.Mutex
	follower := task.Task{
		ID: "task-pet", Status: task.StatusDone, AssignedNode: "pet-box",
		UpdatedAt: time.Now().Add(time.Hour),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		eventsHit.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/ListTasks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode([]task.Task{follower})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	if _, _, err := mgr.Put(task.Task{
		ID: "task-pet", Status: task.StatusTodo, ProjectID: "owner/pet",
		AssignedNode: "pet-box", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	mirror.Run(ctx)

	got, err := mgr.Get("task-pet")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want done — reconcile polling should have applied the follower's state", got.Status)
	}
	if eventsHit.Load() {
		t.Error("Mirror.Run hit /events — it must sync purely by polling ListTasks")
	}
}

// TestMirrorReconcileEscalatesLogLevelOnRepeatedFailure covers #2188: a
// reconcile failure must never be silently invisible again (the previous
// Debug-only log level is exactly how a fleet-wide desync went unnoticed for
// 30+ hours) — Warn on any failure, Error once it's clearly not transient.
func TestMirrorReconcileEscalatesLogLevelOnRepeatedFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized","code":"unauthorized"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mirror := NewMirror(cfg, mgr, roster, logger, 5*time.Millisecond)

	var consecutiveFailures int
	for range reconcileFailureEscalateThreshold - 1 {
		mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)
	}
	beforeThreshold := buf.String()
	if !strings.Contains(beforeThreshold, "level=WARN") {
		t.Errorf("expected a Warn-level reconcile failure log below the threshold, got:\n%s", beforeThreshold)
	}
	if strings.Contains(beforeThreshold, "level=ERROR") {
		t.Errorf("escalated to Error before reaching the %d-failure threshold, got:\n%s", reconcileFailureEscalateThreshold, beforeThreshold)
	}

	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)
	atThreshold := buf.String()
	if !strings.Contains(atThreshold, "level=ERROR") {
		t.Errorf("expected escalation to Error exactly at %d consecutive failures, got:\n%s", reconcileFailureEscalateThreshold, atThreshold)
	}
}

// TestMirrorReconcileMissingTrashesConfirmedGoneTask covers the mirror gap
// from #2294's umbrella-expansion-race post-mortem: a task the leader still
// holds as non-terminal but the follower's ListTasks snapshot omits used to
// stay stuck forever, because reconcileMissing treated every GetTask error —
// including a 404 confirming the task is genuinely gone (e.g. trashed as a
// duplicate cleanup) — the same as a transient failure and never touched the
// canonical copy. That left a ghost task that kept poisoning rollup logic
// scanning all of an umbrella's children, like trackerRollup's
// cancelled-child check. Once the follower confirms the task is gone across
// missingConfirmThreshold consecutive ticks, the leader must trash its own
// stale copy instead.
func TestMirrorReconcileMissingTrashesConfirmedGoneTask(t *testing.T) {
	stub := &followerStub{} // empty tasks/live — GetTask 404s for anything
	srv := stub.server(t)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	if _, _, err := mgr.Put(task.Task{
		ID: "ghost-dup", Status: task.StatusTodo, AssignedNode: "pet-box",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), time.Second)
	var consecutiveFailures int
	for i := 1; i < missingConfirmThreshold; i++ {
		mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)
		if _, err := mgr.Get("ghost-dup"); err != nil {
			t.Fatalf("trashed after only %d confirmation(s), before missingConfirmThreshold (%d)", i, missingConfirmThreshold)
		}
	}
	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)

	if _, err := mgr.Get("ghost-dup"); err == nil {
		t.Fatalf("expected the leader's stale copy to be trashed after %d confirmed-404 ticks", missingConfirmThreshold)
	}
}

// TestMirrorReconcileMissingSurvivesReassignRace covers the exact scenario
// missingConfirmThreshold exists for: Assigner.Reassign stamps
// canonical.AssignedNode to the new node *before* pushing the task there
// (reassign.go's stampNode doc comment — deliberate, so a revived dead
// follower cannot clobber a task that moved), so for at least one reconcile
// tick after a reassignment the new follower legitimately 404s on a task the
// leader now considers assigned to it. A single (or double) 404 must not
// trash the canonical copy; only sustained absence across the full threshold
// may.
func TestMirrorReconcileMissingSurvivesReassignRace(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	if _, _, err := mgr.Put(task.Task{
		ID: "just-reassigned", Status: task.StatusInProgress, AssignedNode: "pet-box",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), time.Second)
	var consecutiveFailures int
	for range missingConfirmThreshold - 1 {
		mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)
	}
	if _, err := mgr.Get("just-reassigned"); err != nil {
		t.Fatalf("canonical copy trashed before the push had a chance to land: %v", err)
	}

	stub.mu.Lock()
	stub.tasks = []task.Task{{
		ID: "just-reassigned", Status: task.StatusInProgress, AssignedNode: "pet-box",
		UpdatedAt: time.Now().Add(time.Minute),
	}}
	stub.mu.Unlock()
	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)

	if _, err := mgr.Get("just-reassigned"); err != nil {
		t.Fatalf("canonical copy must survive once the follower catches up: %v", err)
	}
}

// TestMirrorReconcileMissingResetsStreakOnDirectHit covers a Copilot-review
// finding on this PR: a direct GetTask hit (the follower has the task even
// though this tick's ListTasksForNode snapshot omitted it) must clear any
// in-progress confirmed-404 streak, or a later, unrelated absence inherits
// the stale count and can trash the canonical copy before
// missingConfirmThreshold truly fresh confirmations have accumulated.
func TestMirrorReconcileMissingResetsStreakOnDirectHit(t *testing.T) {
	stub := &followerStub{live: map[string]task.Task{}}
	srv := stub.server(t)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	if _, _, err := mgr.Put(task.Task{
		ID: "flaky-listing", Status: task.StatusInProgress, AssignedNode: "pet-box",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), time.Second)
	var consecutiveFailures int
	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)

	stub.mu.Lock()
	stub.live["flaky-listing"] = task.Task{
		ID: "flaky-listing", Status: task.StatusInProgress, AssignedNode: "pet-box",
		UpdatedAt: time.Now().Add(time.Minute),
	}
	stub.mu.Unlock()
	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)

	stub.mu.Lock()
	delete(stub.live, "flaky-listing")
	stub.mu.Unlock()
	for range missingConfirmThreshold - 1 {
		mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)
	}

	if _, err := mgr.Get("flaky-listing"); err != nil {
		t.Fatalf("canonical copy trashed on a stale pre-reset streak: %v", err)
	}
}

// TestMirrorReconcileMissingLeavesCanonicalOnTransientError is the negative
// case for TestMirrorReconcileMissingTrashesConfirmedGoneTask: a GetTask
// failure that is not a confirmed 404 (follower down, network blip) must
// leave the canonical copy untouched so the next reconcile tick can retry —
// trashing on an ambiguous error would be the #1576 board-wipe failure mode
// recurring in the mirror instead of the local store.
func TestMirrorReconcileMissingLeavesCanonicalOnTransientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/TaskService/ListTasks":
			_ = json.NewEncoder(w).Encode([]task.Task{})
		case "/api/TaskService/GetTask":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom","code":"internal_error"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, err := NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	mgr := newManager(t)
	if _, _, err := mgr.Put(task.Task{
		ID: "maybe-still-there", Status: task.StatusTodo, AssignedNode: "pet-box",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	mirror := NewMirror(cfg, mgr, roster, slog.New(slog.DiscardHandler), time.Second)
	var consecutiveFailures int
	mirror.reconcileNode(context.Background(), "pet-box", &consecutiveFailures)

	got, err := mgr.Get("maybe-still-there")
	if err != nil {
		t.Fatalf("canonical copy must survive a transient GetTask error: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Errorf("status changed on a transient error: %v", got.Status)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
