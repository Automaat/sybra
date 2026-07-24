package sybra

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
)

// followerPushStub is a minimal follower TaskService double for asserting
// what an Assigner push carries — GetTask (PushFieldUpdate's live re-fetch)
// and AssignTask (the actual push).
type followerPushStub struct {
	mu       sync.Mutex
	live     task.Task
	assigned []task.Task
}

func (f *followerPushStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/TaskService/GetTask":
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.live)
		case "/api/TaskService/AssignTask":
			var args []task.Task
			_ = json.Unmarshal(body, &args)
			if len(args) == 1 {
				f.mu.Lock()
				f.assigned = append(f.assigned, args[0])
				f.mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *followerPushStub) lastAssigned() (task.Task, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.assigned) == 0 {
		return task.Task{}, false
	}
	return f.assigned[len(f.assigned)-1], true
}

// TestUpdateTaskPushesTagEditToFollowerAtWriteTime covers #2495: a leader-side
// Tags/DependsOn edit on a task already assigned to its home follower (e.g. a
// manual `sybra-cli update --tags`) must reach the follower at write time
// instead of only via the mirror's detect-and-repair drift backstop one
// reconcile interval later. It also asserts the follower's own live execution
// state (Status, AgentRuns) survives the push untouched — PushFieldUpdate
// patches Tags/DependsOn onto a fresh GetTask, never the leader's own stale
// snapshot.
func TestUpdateTaskPushesTagEditToFollowerAtWriteTime(t *testing.T) {
	stub := &followerPushStub{
		live: task.Task{
			ID:           "task-pet",
			Status:       task.StatusInProgress,
			AssignedNode: "pet-box",
			Tags:         []string{"backend"},
			AgentRuns:    []task.AgentRun{{AgentID: "still-running"}},
		},
	}
	srv := stub.server(t)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "pet-box",
			Endpoints: []string{srv.URL},
			Homes:     []string{"owner/pet"},
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, discardLogger())
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	assigner := clusterlead.NewAssigner(cfg, nil, roster, func(string) bool { return false }, nil, discardLogger())

	leaderTasks := newTaskManagerForMonitorCluster(t)
	if _, _, err := leaderTasks.Put(task.Task{
		ID:           "task-pet",
		Status:       task.StatusInProgress,
		ProjectID:    "owner/pet",
		AssignedNode: "pet-box",
		Tags:         []string{"backend"},
	}); err != nil {
		t.Fatalf("seed leader task: %v", err)
	}

	svc := &TaskService{tasks: leaderTasks, cfg: cfg, assigner: assigner, logger: discardLogger(), wg: &sync.WaitGroup{}}

	if _, err := svc.UpdateTask("task-pet", map[string]any{"tags": []string{"backend", "release-blocked"}}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	// The follower push runs detached (s.wg.Go) so UpdateTask never blocks the
	// Wails caller; wait for it to land before asserting on the follower.
	svc.wg.Wait()

	got, ok := stub.lastAssigned()
	if !ok {
		t.Fatal("follower did not receive a write-time push for the tags edit")
	}
	if len(got.Tags) != 2 || got.Tags[0] != "backend" || got.Tags[1] != "release-blocked" {
		t.Errorf("pushed tags = %v, want [backend release-blocked]", got.Tags)
	}
	if got.Status != task.StatusInProgress {
		t.Errorf("push overwrote follower status: got %q, want %q (must not roll back execution state)", got.Status, task.StatusInProgress)
	}
	if len(got.AgentRuns) != 1 || got.AgentRuns[0].AgentID != "still-running" {
		t.Errorf("push dropped the follower's live AgentRuns, got %+v — rolled back follower progress", got.AgentRuns)
	}
}

// TestUpdateTaskSkipsFollowerPushForLocalTask asserts the write-time push is
// a no-op for a task that isn't homed on any follower — the common case for
// every single-node install, which must not pay any extra network cost.
func TestUpdateTaskSkipsFollowerPushForLocalTask(t *testing.T) {
	cfg := &config.Config{}
	roster, err := clusterlead.NewRoster(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	assigner := clusterlead.NewAssigner(cfg, nil, roster, func(string) bool { return false }, nil, discardLogger())

	leaderTasks := newTaskManagerForMonitorCluster(t)
	if _, _, err := leaderTasks.Put(task.Task{
		ID: "task-local", Status: task.StatusTodo, Tags: []string{"backend"},
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	svc := &TaskService{tasks: leaderTasks, cfg: cfg, assigner: assigner, logger: discardLogger(), wg: &sync.WaitGroup{}}
	if _, err := svc.UpdateTask("task-local", map[string]any{"tags": []string{"backend", "extra"}}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	svc.wg.Wait()
	got, err := leaderTasks.Get("task-local")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("local update did not apply: %+v", got.Tags)
	}
}
