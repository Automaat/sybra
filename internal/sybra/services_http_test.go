package sybra

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/learning"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
)

// TestLearningServiceHTTPAllowlist verifies the confidentiality contract for
// the raw learning store: ListDigests and GetLatestDigest are reachable over
// HTTP, but StoreDigest (which writes unscrubbed digests) is not.
func TestLearningServiceHTTPAllowlist(t *testing.T) {
	store, err := learning.New(t.TempDir())
	if err != nil {
		t.Fatalf("learning.New: %v", err)
	}
	a := &App{learningSvc: &LearningService{store: store}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	post := func(method string) int {
		resp, err := http.Post(srv.URL+"/api/LearningService/"+method, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if status := post("ListDigests"); status == http.StatusNotFound {
		t.Errorf("ListDigests should be reachable over HTTP, got %d", status)
	}
	if status := post("GetLatestDigest"); status == http.StatusNotFound {
		t.Errorf("GetLatestDigest should be reachable over HTTP, got %d", status)
	}
	if status := post("StoreDigest"); status != http.StatusNotFound {
		t.Errorf("StoreDigest must NOT be reachable over HTTP, got %d (want 404)", status)
	}
}

func TestQueueServiceHTTPAllowlist(t *testing.T) {
	queue, err := agentqueue.New(t.TempDir(), agentqueue.Options{}, slog.Default())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}
	a := &App{queueSvc: &QueueService{queue: queue}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	post := func(method string) int {
		resp, err := http.Post(srv.URL+"/api/QueueService/"+method, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if status := post("SnapshotDepth"); status == http.StatusNotFound {
		t.Errorf("SnapshotDepth should be reachable over HTTP, got %d", status)
	}
	if status := post("AgentQueueSnapshot"); status != http.StatusNotFound {
		t.Errorf("QueueService.AgentQueueSnapshot must stay off the QueueService HTTP surface, got %d (want 404)", status)
	}
	if status := post("Snapshot"); status != http.StatusNotFound {
		t.Errorf("Snapshot must NOT be reachable over HTTP, got %d (want 404)", status)
	}
}

func TestAppHTTPAllowlist(t *testing.T) {
	queue, err := agentqueue.New(t.TempDir(), agentqueue.Options{}, slog.Default())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}
	if added := queue.Offer(agentqueue.Item{
		TaskID:   "queued-task",
		Priority: task.PriorityNone,
		Status:   task.StatusInReview,
		Mode:     "headless",
	}); !added {
		t.Fatal("Offer(queued-task) returned false, want true")
	}
	a := &App{queueSvc: &QueueService{queue: queue}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/App/AgentQueueSnapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST AgentQueueSnapshot: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("AgentQueueSnapshot status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var got AgentQueueSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode AgentQueueSnapshot response: %v", err)
	}
	if got.Depth != 1 || len(got.Items) != 1 {
		t.Fatalf("AgentQueueSnapshot response = %+v, want single queued item", got)
	}
	if got.Items[0].TaskID != "queued-task" {
		t.Fatalf("AgentQueueSnapshot item task = %q, want queued-task", got.Items[0].TaskID)
	}
	if got.Items[0].EffectivePriority != string(task.PriorityHigh) {
		t.Fatalf("AgentQueueSnapshot effective priority = %q, want %q", got.Items[0].EffectivePriority, task.PriorityHigh)
	}
}

// TestTaskServiceHTTPAllowlist_ListTaskProgress guards against the Progress
// tab regressing to unreachable-over-HTTP: the method exists on TaskService
// and works in-process (Wails desktop), but was never added to the
// coreHTTPServices allowlist, so the server/web surface 404'd on it.
func TestTaskServiceHTTPAllowlist_ListTaskProgress(t *testing.T) {
	a := &App{taskSvc: &TaskService{}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/TaskService/ListTaskProgress", "application/json", strings.NewReader(`["any"]`))
	if err != nil {
		t.Fatalf("POST ListTaskProgress: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ListTaskProgress status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var entries []artifact.ProgressEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode ListTaskProgress response: %v", err)
	}
	if entries == nil {
		t.Fatal("ListTaskProgress response decoded to nil, want empty slice")
	}
}

// TestPromptLabServiceHTTPAllowlist verifies ApproveProposal/RejectProposal
// are reachable over HTTP (web/server surface, not just Wails desktop), and
// that the requirePendingProposal guard rejects a non-pending task with no
// mutation over the HTTP path too.
func TestPromptLabServiceHTTPAllowlist(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	mgr := task.NewManager(store, nil)
	artifacts := artifact.New(t.TempDir())
	a := &App{promptLabSvc: &PromptLabService{tasks: mgr, artifacts: artifacts}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	staleStatus := task.StatusTodo
	staleTags := []string{promptlab.ProposalTag}
	stale, err := mgr.CreateFull("Stale proposal", "body", task.AgentModeInteractive, task.Update{
		Status: &staleStatus,
		Tags:   &staleTags,
	})
	if err != nil {
		t.Fatalf("create stale proposal: %v", err)
	}

	resp, err := http.Post(srv.URL+"/api/PromptLabService/ApproveProposal", "application/json", strings.NewReader(`["`+stale.ID+`"]`))
	if err != nil {
		t.Fatalf("POST ApproveProposal: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("ApproveProposal should be reachable over HTTP, got 404")
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("ApproveProposal on non-pending task should error, got %d", resp.StatusCode)
	}
	after, err := mgr.Get(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != staleStatus {
		t.Fatalf("status mutated to %q by guard-rejected HTTP call, want unchanged %q", after.Status, staleStatus)
	}

	pendingStatus := task.StatusHumanRequired
	pendingTags := []string{promptlab.ProposalTag, "requires-human"}
	pending, err := mgr.CreateFull("Pending proposal", "body", task.AgentModeInteractive, task.Update{
		Status: &pendingStatus,
		Tags:   &pendingTags,
	})
	if err != nil {
		t.Fatalf("create pending proposal: %v", err)
	}

	resp2, err := http.Post(srv.URL+"/api/PromptLabService/RejectProposal", "application/json", strings.NewReader(`["`+pending.ID+`", "no thanks"]`))
	if err != nil {
		t.Fatalf("POST RejectProposal: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("RejectProposal status = %d, want %d", resp2.StatusCode, http.StatusOK)
	}
	var got task.Task
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatalf("decode RejectProposal response: %v", err)
	}
	if got.Status != task.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}
