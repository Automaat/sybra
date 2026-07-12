package clusterlead

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

type followerStub struct {
	mu       sync.Mutex
	assigned []task.Task
	tasks    []task.Task
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
	assigner := NewAssigner(cfg, mgr, roster, nil)

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

func TestAssignerLeavesLocalTaskAlone(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := leaderConfig(srv.URL, []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, nil)

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
	assigner := NewAssigner(cfg, mgr, roster, nil)

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

func TestMirrorIgnoresUnownedTask(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if _, _, err := mgr.Put(task.Task{ID: "task-x", Status: task.StatusTodo, AssignedNode: "other-node", UpdatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	follower := task.Task{ID: "task-x", Status: task.StatusDone, UpdatedAt: time.Unix(100, 0)}
	if mirror.applyFollowerTask("pet-box", follower) {
		t.Error("a task not assigned to this node must not be mirrored from it")
	}

	if mirror.applyFollowerTask("pet-box", task.Task{ID: "ghost", UpdatedAt: time.Unix(100, 0)}) {
		t.Error("a task the leader does not own must be ignored")
	}
}
