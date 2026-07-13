package clusterlead

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

type followerRecorder struct {
	mu       sync.Mutex
	assigned []task.Task
	stopped  []string
	agents   []map[string]any
}

func (f *followerRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/TaskService/AssignTask", func(w http.ResponseWriter, r *http.Request) {
		var args []task.Task
		_ = json.NewDecoder(r.Body).Decode(&args)
		f.mu.Lock()
		if len(args) > 0 {
			f.assigned = append(f.assigned, args[0])
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`null`))
	})
	mux.HandleFunc("/api/AgentService/ListAgents", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		agents := f.agents
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agents)
	})
	mux.HandleFunc("/api/AgentService/StopAgent", func(w http.ResponseWriter, r *http.Request) {
		var args []string
		_ = json.NewDecoder(r.Body).Decode(&args)
		f.mu.Lock()
		if len(args) > 0 {
			f.stopped = append(f.stopped, args[0])
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`null`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *followerRecorder) assignedTasks() []task.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]task.Task(nil), f.assigned...)
}

func (f *followerRecorder) stoppedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func newReassignFixture(t *testing.T, followers []config.Follower, isWork func(string) bool) (*Assigner, *task.Manager) {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)
	cfg := &config.Config{Cluster: config.ClusterConfig{Role: config.ClusterRoleLeader, Followers: followers}}
	roster, err := NewRoster(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	if isWork == nil {
		isWork = func(string) bool { return false }
	}
	return NewAssigner(cfg, tasks, roster, isWork, nil, slog.Default()), tasks
}

func seedTask(t *testing.T, tasks *task.Manager, seed task.Task) task.Task {
	t.Helper()
	saved, _, err := tasks.Put(seed)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return saved
}

func TestReassignRepushesToNewNodeAndClearsWorktree(t *testing.T) {
	oldNode := &followerRecorder{}
	newNode := &followerRecorder{}
	oldSrv := oldNode.server(t)
	newSrv := newNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)

	mirrored := time.Now()
	seedTask(t, tasks, task.Task{
		ID:              "t1",
		Title:           "move me",
		ProjectID:       "acme/x",
		Status:          task.StatusInProgress,
		AssignedNode:    "old-box",
		WorktreeDir:     "/on/old/box/wt",
		Branch:          "feat/x",
		MirrorRev:       7,
		MirrorUpdatedAt: &mirrored,
	})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}

	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "new-box" {
		t.Fatalf("assigned_node = %q, want new-box", got.AssignedNode)
	}
	if got.WorktreeDir != "" {
		t.Fatalf("worktree must be cleared (it is not transferable), got %q", got.WorktreeDir)
	}
	if got.Branch != "feat/x" {
		t.Fatalf("branch is the handoff artifact and must survive, got %q", got.Branch)
	}
	if got.MirrorRev != 0 || got.MirrorUpdatedAt != nil {
		t.Fatalf("mirror clock must reset for the new node, got rev=%d at=%v", got.MirrorRev, got.MirrorUpdatedAt)
	}

	pushed := newNode.assignedTasks()
	if len(pushed) != 1 || pushed[0].ID != "t1" {
		t.Fatalf("task was not pushed to the new node: %+v", pushed)
	}
	if pushed[0].AssignedNode != "new-box" {
		t.Fatalf("pushed task carries node %q, want new-box", pushed[0].AssignedNode)
	}
	if got := oldNode.assignedTasks(); len(got) != 0 {
		t.Fatalf("task must not be re-pushed to the old node: %+v", got)
	}
}

func TestReassignStopsAgentsOnTheOldNode(t *testing.T) {
	oldNode := &followerRecorder{agents: []map[string]any{
		{"id": "a-mine", "taskId": "t1", "state": "running"},
		{"id": "a-other", "taskId": "t2", "state": "running"},
	}}
	newNode := &followerRecorder{}
	oldSrv := oldNode.server(t)
	newSrv := newNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", ProjectID: "acme/x", Status: task.StatusInProgress, AssignedNode: "old-box"})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}

	stopped := oldNode.stoppedAgents()
	if len(stopped) != 1 || stopped[0] != "a-mine" {
		t.Fatalf("the old node's agent for this task must be stopped (else two agents drive one branch), stopped=%v", stopped)
	}
}

func TestReassignProceedsWhenOldNodeIsDead(t *testing.T) {
	newNode := &followerRecorder{}
	newSrv := newNode.server(t)
	dead := httptest.NewServer(http.NewServeMux())
	dead.Close()

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "dead-box", Endpoints: []string{dead.URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", ProjectID: "acme/x", Status: task.StatusInProgress, AssignedNode: "dead-box"})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("a dead old node must not block the escape hatch: %v", err)
	}
	if pushed := newNode.assignedTasks(); len(pushed) != 1 {
		t.Fatalf("task was not moved off the dead node: %+v", pushed)
	}
}

func TestReassignHomeBringsTaskBackToLeader(t *testing.T) {
	oldNode := &followerRecorder{}
	oldSrv := oldNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", ProjectID: "acme/x", Status: task.StatusInProgress, AssignedNode: "old-box", WorktreeDir: "/remote/wt"})

	if err := a.Reassign(t.Context(), "t1", config.LocalNodeName); err != nil {
		t.Fatalf("Reassign local: %v", err)
	}
	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "" {
		t.Fatalf("a task brought home must have an empty assigned_node (that is what the local dispatch gate reads), got %q", got.AssignedNode)
	}
	if got.WorktreeDir != "" {
		t.Fatalf("worktree must be cleared, got %q", got.WorktreeDir)
	}
}

func TestReassignRejectsUnknownNode(t *testing.T) {
	a, tasks := newReassignFixture(t, nil, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusTodo})

	err := a.Reassign(t.Context(), "t1", "ghost")
	if !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("want ErrUnknownNode, got %v", err)
	}
	got, _ := tasks.Get("t1")
	if got.AssignedNode != "" {
		t.Fatalf("a rejected reassignment must not move the task, got node %q", got.AssignedNode)
	}
}

func TestReassignCannotBypassConfidentialityGuard(t *testing.T) {
	untrusted := &followerRecorder{}
	srv := untrusted.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "untrusted-box", Endpoints: []string{srv.URL}, Trusted: false, Homes: []string{"acme/private"}},
	}, func(string) bool { return true })
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusTodo, ProjectID: "acme/private"})

	err := a.Reassign(t.Context(), "t1", "untrusted-box")
	if !errors.Is(err, ErrConfidentiality) {
		t.Fatalf("a manual reassignment must not be a way around the work-data guard, got %v", err)
	}
	if pushed := untrusted.assignedTasks(); len(pushed) != 0 {
		t.Fatalf("work task was pushed to an untrusted node: %+v", pushed)
	}
	got, _ := tasks.Get("t1")
	if got.AssignedNode != "" {
		t.Fatalf("blocked reassignment must not stamp the node, got %q", got.AssignedNode)
	}
}

func TestReassignIsIdempotentForTheSameNode(t *testing.T) {
	node := &followerRecorder{}
	srv := node.server(t)
	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "box", Endpoints: []string{srv.URL}, Trusted: true, Homes: []string{"acme/x"}},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", ProjectID: "acme/x", Status: task.StatusInProgress, AssignedNode: "box", WorktreeDir: "/wt"})

	if err := a.Reassign(t.Context(), "t1", "box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if pushed := node.assignedTasks(); len(pushed) != 0 {
		t.Fatalf("reassigning to the node it already runs on must be a no-op, not a re-push: %+v", pushed)
	}
	got, _ := tasks.Get("t1")
	if got.WorktreeDir != "/wt" {
		t.Fatalf("a no-op reassignment must not destroy the live worktree, got %q", got.WorktreeDir)
	}
}

func TestReassignedTaskIgnoresStaleMirrorFromOldNode(t *testing.T) {
	oldNode := &followerRecorder{}
	newNode := &followerRecorder{}
	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldNode.server(t).URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "new-box", Endpoints: []string{newNode.server(t).URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", ProjectID: "acme/x", Status: task.StatusInProgress, AssignedNode: "old-box"})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}

	canonical, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	cfg := &config.Config{Cluster: config.ClusterConfig{Role: config.ClusterRoleLeader}}
	mirror := NewMirror(cfg, tasks, a.roster, slog.Default(), 0)

	revived := canonical
	revived.AssignedNode = "old-box"
	revived.Status = task.StatusDone
	revived.UpdatedAt = time.Now().Add(time.Hour)

	if applied := mirror.applyFollowerTask("old-box", revived); applied {
		t.Fatal("a dead follower that comes back must not clobber a task that has moved")
	}
	after, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status == task.StatusDone {
		t.Fatalf("stale mirror from the old node overwrote canonical status: %s", after.Status)
	}
	if after.AssignedNode != "new-box" {
		t.Fatalf("stale mirror stole the task back to %q", after.AssignedNode)
	}
}

func TestClusterNodesListsFollowers(t *testing.T) {
	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{
			{Name: "pet-box", Endpoints: []string{"https://pet.example.ts.net"}, Trusted: true, Homes: []string{"a/b"}},
		},
	}}
	home, ok := cfg.HomeNodeByName("pet-box")
	if !ok {
		t.Fatal("HomeNodeByName missed a configured follower")
	}
	if !home.Trusted || !home.Encrypted {
		t.Fatalf("https follower must resolve trusted+encrypted, got %+v", home)
	}
	if _, ok := cfg.HomeNodeByName("nope"); ok {
		t.Fatal("unknown node must not resolve")
	}
	local, ok := cfg.HomeNodeByName(config.LocalNodeName)
	if !ok || !local.Local || local.Name != "" {
		t.Fatalf("local must resolve to the leader with an empty name, got %+v", local)
	}
}

func TestReassignUnknownNodeMessageNamesTheNode(t *testing.T) {
	a, tasks := newReassignFixture(t, nil, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusTodo})
	err := a.Reassign(context.Background(), "t1", "ghost")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("operator needs to see which node was rejected, got %v", err)
	}
}

func TestReassignSurvivesTheNextAssignerTick(t *testing.T) {
	oldNode := &followerRecorder{}
	newNode := &followerRecorder{}
	oldSrv := oldNode.server(t)
	newSrv := newNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{
		ID: "t1", Title: "x", ProjectID: "acme/x",
		Status: task.StatusInProgress, AssignedNode: "old-box",
	})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	a.Tick(t.Context())

	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "new-box" {
		t.Fatalf("the assigner dragged the task back to its config home: node=%q", got.AssignedNode)
	}
	if len(oldNode.assignedTasks()) != 0 {
		t.Fatalf("the assigner re-pushed the task to the node it was evacuated from: %+v", oldNode.assignedTasks())
	}
}

func TestBringHomeSurvivesTheNextAssignerTick(t *testing.T) {
	oldNode := &followerRecorder{}
	oldSrv := oldNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
	}, nil)
	seedTask(t, tasks, task.Task{
		ID: "t1", Title: "x", ProjectID: "acme/x",
		Status: task.StatusInProgress, AssignedNode: "old-box",
	})

	if err := a.Reassign(t.Context(), "t1", config.LocalNodeName); err != nil {
		t.Fatalf("Reassign local: %v", err)
	}
	a.Tick(t.Context())

	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "" {
		t.Fatalf("a task brought home was re-routed to %q", got.AssignedNode)
	}
	if len(oldNode.assignedTasks()) != 0 {
		t.Fatalf("a task brought home was pushed back to a follower: %+v", oldNode.assignedTasks())
	}
	if !a.cfg.HomeNodeForTask(got.ProjectID, got.NodeOverride).Local {
		t.Fatal("the leader's dispatch gates read HomeNodeForTask; a task brought home must resolve local")
	}
}

func TestReassignRollsBackWhenTheNewNodeRejectsThePush(t *testing.T) {
	oldNode := &followerRecorder{}
	oldSrv := oldNode.server(t)
	deadNew := httptest.NewServer(http.NewServeMux())
	deadNew.Close()

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "old-box", Endpoints: []string{oldSrv.URL}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "sick-box", Endpoints: []string{deadNew.URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{
		ID: "t1", Title: "x", ProjectID: "acme/x",
		Status: task.StatusInProgress, AssignedNode: "old-box",
	})

	if err := a.Reassign(t.Context(), "t1", "sick-box"); err == nil {
		t.Fatal("want an error when the target node cannot be reached")
	}
	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "old-box" || got.NodeOverride != "" {
		t.Fatalf("a failed push must not strand the task on a node that never received it: node=%q override=%q",
			got.AssignedNode, got.NodeOverride)
	}
}

func TestReassignStopsTheLeadersOwnAgentsWhenMovingOffLocal(t *testing.T) {
	newNode := &followerRecorder{}
	newSrv := newNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)
	var stopped []string
	a.SetStopLocalAgents(func(taskID string) { stopped = append(stopped, taskID) })

	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusInProgress})

	if err := a.Reassign(t.Context(), "t1", "new-box"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != "t1" {
		t.Fatalf("the leader's own agent must be stopped before a follower starts one on the same branch, stopped=%v", stopped)
	}
}

func TestReassignRefusesToMoveALocalRunWhenItCannotDrainIt(t *testing.T) {
	newNode := &followerRecorder{}
	newSrv := newNode.server(t)

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "new-box", Endpoints: []string{newSrv.URL}, Trusted: true},
	}, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusInProgress})

	err := a.Reassign(t.Context(), "t1", "new-box")
	if !errors.Is(err, ErrCannotDrainLocal) {
		t.Fatalf("a caller with no way to stop the leader's agents must fail closed, got %v", err)
	}
	if len(newNode.assignedTasks()) != 0 {
		t.Fatalf("task was pushed while a local agent may still drive the branch: %+v", newNode.assignedTasks())
	}
}

func TestReassignRejectsEmptyNode(t *testing.T) {
	a, tasks := newReassignFixture(t, nil, nil)
	seedTask(t, tasks, task.Task{ID: "t1", Title: "x", Status: task.StatusTodo})
	if err := a.Reassign(t.Context(), "t1", "   "); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("a blank node must be rejected, not resolved to an unnamed follower, got %v", err)
	}
}

func TestRollbackKeepsTheOperatorsPreviousPin(t *testing.T) {
	nodeB := &followerRecorder{}
	bSrv := nodeB.server(t)
	sick := httptest.NewServer(http.NewServeMux())
	sick.Close()

	a, tasks := newReassignFixture(t, []config.Follower{
		{Name: "node-a", Endpoints: []string{"http://127.0.0.1:1"}, Trusted: true, Homes: []string{"acme/x"}},
		{Name: "node-b", Endpoints: []string{bSrv.URL}, Trusted: true},
		{Name: "node-c", Endpoints: []string{sick.URL}, Trusted: true},
	}, nil)

	seedTask(t, tasks, task.Task{
		ID: "t1", Title: "x", ProjectID: "acme/x",
		Status: task.StatusInProgress, AssignedNode: "node-b", NodeOverride: "node-b",
	})

	if err := a.Reassign(t.Context(), "t1", "node-c"); err == nil {
		t.Fatal("want an error when the target refuses the push")
	}

	got, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AssignedNode != "node-b" || got.NodeOverride != "node-b" {
		t.Fatalf("a failed push must restore the operator's pin, got node=%q override=%q", got.AssignedNode, got.NodeOverride)
	}

	a.Tick(t.Context())
	after, err := tasks.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.AssignedNode != "node-b" {
		t.Fatalf("after a failed push the assigner dragged the task back to its config home: %q", after.AssignedNode)
	}
}
