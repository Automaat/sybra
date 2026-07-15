package sybra

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
)

type noopAuditReader struct{}

func (noopAuditReader) Read(audit.Query) ([]audit.Event, error) { return nil, nil }

type staticProjectGetter struct{}

func (staticProjectGetter) Get(string) (project.Project, error) {
	return project.Project{Type: project.ProjectTypePet}, nil
}

type recoveringOrchestrator struct {
	mu        sync.Mutex
	tasks     *task.Manager
	startedAt time.Time
	started   []string
}

func (o *recoveringOrchestrator) StartAgent(taskID, mode, prompt string, includeTaskDescription, oneShot bool) (*agent.Agent, error) {
	o.mu.Lock()
	o.started = append(o.started, taskID)
	o.mu.Unlock()
	runID := "recovered-" + taskID
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   runID,
		Mode:      mode,
		State:     string(agent.StateRunning),
		StartedAt: o.startedAt,
		Provider:  "claude",
		Prompt:    prompt,
	}); err != nil {
		return nil, err
	}
	return &agent.Agent{
		ID:          runID,
		TaskID:      taskID,
		Mode:        mode,
		State:       agent.StateRunning,
		StartedAt:   o.startedAt,
		LastEventAt: o.startedAt,
	}, nil
}

func (*recoveringOrchestrator) StartPRFixAgent(string) error { return nil }

func (o *recoveringOrchestrator) startedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.started)
}

func newTaskManagerForMonitorCluster(t *testing.T) *task.Manager {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return task.NewManager(store, task.EmitterFunc(func(string, any) {}))
}

func TestLeaderMonitorRoutesRemoteLostAgentAndMirrorConverges(t *testing.T) {
	now := time.Now().UTC()
	followerTasks := newTaskManagerForMonitorCluster(t)
	staleRun := task.AgentRun{
		AgentID:   "remote-old",
		Mode:      task.AgentModeHeadless,
		State:     string(agent.StateRunning),
		StartedAt: now.Add(-2 * time.Hour),
		Provider:  "claude",
	}
	remote := task.Task{
		ID:              "remote-1",
		Title:           "Remote task",
		Status:          task.StatusInProgress,
		AgentMode:       task.AgentModeHeadless,
		ProjectID:       "owner/pet",
		AssignedNode:    "pet-box",
		CreatedAt:       now.Add(-3 * time.Hour),
		UpdatedAt:       now.Add(-2 * time.Hour),
		StatusChangedAt: now.Add(-2 * time.Hour),
		AgentRuns:       []task.AgentRun{staleRun},
	}
	if _, _, err := followerTasks.Put(remote); err != nil {
		t.Fatalf("seed follower task: %v", err)
	}

	followerAgents := newTestAgentManager(t, t.Context(), func(string, any) {}, discardLogger(), t.TempDir())
	orch := &recoveringOrchestrator{tasks: followerTasks, startedAt: now}
	followerRecovery := &recovery.Recovery{
		Tasks:        followerTasks,
		Agents:       followerAgents,
		Orchestrator: orch,
		Projects:     staticProjectGetter{},
		Logger:       discardLogger(),
		Throttle:     logging.NewErrorThrottle(),
		WG:           &sync.WaitGroup{},
		DispatchGate: func(task.Task) bool { return true },
	}

	followerTaskSvc := &TaskService{
		tasks:            followerTasks,
		agents:           followerAgents,
		logger:           discardLogger(),
		recoverLostAgent: followerRecovery.RestartTaskIfStale,
	}
	followerAgentSvc := &AgentService{agents: followerAgents}

	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService":  httpapi.NewService(followerTaskSvc, "AssignTask", "RecoverLostAgent", "ListTasks", "GetTask"),
		"AgentService": httpapi.NewService(followerAgentSvc, "ListAgents"),
	}, slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Monitor: config.DefaultConfig().Monitor,
		Cluster: config.ClusterConfig{
			Role: config.ClusterRoleLeader,
			Followers: []config.Follower{{
				Name:      "pet-box",
				Endpoints: []string{srv.URL},
				Homes:     []string{"owner/pet"},
			}},
		},
	}
	roster, err := clusterlead.NewRoster(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}

	leaderTasks := newTaskManagerForMonitorCluster(t)
	if _, _, err := leaderTasks.Put(task.Task{
		ID:              remote.ID,
		Title:           remote.Title,
		Status:          remote.Status,
		AgentMode:       remote.AgentMode,
		ProjectID:       remote.ProjectID,
		AssignedNode:    remote.AssignedNode,
		CreatedAt:       remote.CreatedAt,
		UpdatedAt:       remote.CreatedAt,
		StatusChangedAt: remote.StatusChangedAt,
	}); err != nil {
		t.Fatalf("seed leader task: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mirror := clusterlead.NewMirror(cfg, leaderTasks, roster, discardLogger(), 10*time.Millisecond)
	go mirror.Run(ctx)

	waitForMonitorCluster(t, "initial mirror sync", func() bool {
		got, err := leaderTasks.Get(remote.ID)
		return err == nil && len(got.AgentRuns) == 1 && got.MirrorRev >= 1
	})

	leaderApp := &App{
		tasks:         leaderTasks,
		cfg:           cfg,
		clusterRoster: roster,
		logger:        discardLogger(),
	}
	monCfg := cfg.Monitor
	monCfg.Enabled = true
	monCfg.LostAgentMinutes = 5
	monCfg.DispatchLimit = 100
	svc := monitor.NewService(monitor.Deps{
		Cfg:        monCfg,
		Tasks:      leaderTasks,
		Audit:      noopAuditReader{},
		Agents:     newMonitorAgentLister(nil, roster, discardLogger()),
		Dispatcher: monitor.NoopDispatcher(),
		Sink:       monitor.NoopSink(),
		Logger:     discardLogger(),
		Now:        func() time.Time { return now },
		RecoverLostAgent: func(ctx context.Context, taskID string) {
			leaderApp.recoverLostAgentTask(ctx, taskID)
		},
	})
	runCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	go svc.Run(runCtx)

	var report monitor.Report
	waitForMonitorCluster(t, "monitor remediation", func() bool {
		var ok bool
		report, ok = svc.LastReport()
		if !ok {
			return false
		}
		return len(report.Remediated) == 1
	})
	stopMonitor()
	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != monitor.KindLostAgent {
		t.Fatalf("anomalies = %v, want one lost_agent", report.Anomalies)
	}
	if len(report.Remediated) != 1 || report.Remediated[0] != "lost_agent:"+remote.ID {
		t.Fatalf("remediated = %v, want lost_agent:%s", report.Remediated, remote.ID)
	}
	if orch.startedCount() != 1 {
		t.Fatalf("remote recovery starts = %d, want 1", orch.startedCount())
	}

	waitForMonitorCluster(t, "mirror convergence after remote recovery", func() bool {
		got, err := leaderTasks.Get(remote.ID)
		if err != nil {
			return false
		}
		if got.MirrorRev < 2 || len(got.AgentRuns) < 2 {
			return false
		}
		last := got.AgentRuns[len(got.AgentRuns)-1]
		first := got.AgentRuns[0]
		return last.AgentID == "recovered-"+remote.ID &&
			last.State == string(agent.StateRunning) &&
			first.State == string(agent.StateStopped) &&
			got.StatusReason == "monitor: agent lost; recovery will resume"
	})

	got, err := leaderTasks.Get(remote.ID)
	if err != nil {
		t.Fatalf("Get leader task: %v", err)
	}
	if got.AssignedNode != "pet-box" {
		t.Fatalf("AssignedNode = %q, want pet-box", got.AssignedNode)
	}
	if got.MirrorRev < 2 {
		t.Fatalf("MirrorRev = %d, want >=2 after recovery convergence", got.MirrorRev)
	}
}

func waitForMonitorCluster(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
