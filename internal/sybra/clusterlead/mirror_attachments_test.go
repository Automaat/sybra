package clusterlead

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

func TestMirrorPullsFollowerAttachmentBlobsToLeader(t *testing.T) {
	var mu sync.Mutex
	var exported []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ClusterAttachmentService/ExportAttachment" {
			http.NotFound(w, r)
			return
		}
		var args []string
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Fatalf("decode export args: %v", err)
		}
		mu.Lock()
		exported = append(exported, strings.Join(args, "/"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]byte("follower payload"))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "pet-box",
			Endpoints: []string{srv.URL},
			Trusted:   true,
		}},
	}}
	roster, err := NewRoster(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)
	leaderAttachments, err := attachment.NewStore(t.TempDir(), 10<<20)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	mirror := NewMirror(cfg, tasks, roster, slog.Default(), time.Second)
	mirror.SetAttachments(leaderAttachments)

	t0 := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if _, _, err := tasks.Put(task.Task{
		ID:           "task-1",
		Title:        "mirror attachment",
		Status:       task.StatusInProgress,
		AgentMode:    task.AgentModeHeadless,
		AssignedNode: "pet-box",
		CreatedAt:    t0,
		UpdatedAt:    t0,
	}); err != nil {
		t.Fatalf("seed canonical task: %v", err)
	}

	followerUpdated := t0.Add(time.Hour)
	if !mirror.applyFollowerTaskWithContext(t.Context(), "pet-box", task.Task{
		ID:           "task-1",
		Title:        "mirror attachment",
		Status:       task.StatusInProgress,
		AgentMode:    task.AgentModeHeadless,
		AssignedNode: "pet-box",
		UpdatedAt:    followerUpdated,
		Attachments: []task.Attachment{{
			ID:          "att_1",
			FileName:    "evidence.txt",
			ContentType: "text/plain",
			SizeBytes:   16,
			Path:        "/follower/attachments/task-1/att_1/evidence.txt",
			CreatedAt:   followerUpdated,
		}},
	}) {
		t.Fatal("mirror update was not applied")
	}

	mu.Lock()
	gotExported := slices.Clone(exported)
	mu.Unlock()
	if len(gotExported) != 1 || gotExported[0] != "task-1/att_1" {
		t.Fatalf("exported = %v, want task-1/att_1", gotExported)
	}
	got, err := tasks.Get("task-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("leader attachments = %+v, want one", got.Attachments)
	}
	if strings.Contains(got.Attachments[0].Path, "/follower/") {
		t.Fatalf("leader kept follower-local path: %q", got.Attachments[0].Path)
	}
	data, err := os.ReadFile(got.Attachments[0].Path)
	if err != nil {
		t.Fatalf("read mirrored attachment: %v", err)
	}
	if string(data) != "follower payload" {
		t.Fatalf("mirrored payload = %q, want follower payload", data)
	}
}

func TestMirrorRetriesFollowerAttachmentAfterExportFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ClusterAttachmentService/ExportAttachment" {
			http.NotFound(w, r)
			return
		}
		var args []string
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Fatalf("decode export args: %v", err)
		}
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			http.Error(w, "temporary export failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]byte("eventual payload"))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "pet-box",
			Endpoints: []string{srv.URL},
			Trusted:   true,
		}},
	}}
	roster, err := NewRoster(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, nil)
	leaderAttachments, err := attachment.NewStore(t.TempDir(), 10<<20)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	mirror := NewMirror(cfg, tasks, roster, slog.Default(), time.Second)
	mirror.SetAttachments(leaderAttachments)

	t0 := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if _, _, err := tasks.Put(task.Task{
		ID:           "task-1",
		Title:        "mirror attachment",
		Status:       task.StatusInProgress,
		AgentMode:    task.AgentModeHeadless,
		AssignedNode: "pet-box",
		CreatedAt:    t0,
		UpdatedAt:    t0,
	}); err != nil {
		t.Fatalf("seed canonical task: %v", err)
	}

	followerUpdated := t0.Add(time.Hour)
	follower := task.Task{
		ID:           "task-1",
		Title:        "mirror attachment",
		Status:       task.StatusInProgress,
		AgentMode:    task.AgentModeHeadless,
		AssignedNode: "pet-box",
		UpdatedAt:    followerUpdated,
		Attachments: []task.Attachment{{
			ID:          "att_1",
			FileName:    "evidence.txt",
			ContentType: "text/plain",
			SizeBytes:   16,
			Path:        "/follower/attachments/task-1/att_1/evidence.txt",
			CreatedAt:   followerUpdated,
		}},
	}

	if mirror.applyFollowerTaskWithContext(t.Context(), "pet-box", follower) {
		t.Fatal("mirror must not apply a follower update whose attachment blob could not be copied")
	}
	got, err := tasks.Get("task-1")
	if err != nil {
		t.Fatalf("Get after failed mirror: %v", err)
	}
	if got.MirrorUpdatedAt != nil {
		t.Fatalf("failed attachment mirror advanced MirrorUpdatedAt to %v", got.MirrorUpdatedAt)
	}
	if len(got.Attachments) != 0 {
		t.Fatalf("failed attachment mirror persisted attachments = %+v, want none", got.Attachments)
	}

	if !mirror.applyFollowerTaskWithContext(t.Context(), "pet-box", follower) {
		t.Fatal("mirror did not retry the same follower update after export recovered")
	}
	got, err = tasks.Get("task-1")
	if err != nil {
		t.Fatalf("Get after retry: %v", err)
	}
	if got.MirrorUpdatedAt == nil || !got.MirrorUpdatedAt.Equal(followerUpdated) {
		t.Fatalf("MirrorUpdatedAt after retry = %v, want %v", got.MirrorUpdatedAt, followerUpdated)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("leader attachments after retry = %+v, want one", got.Attachments)
	}
	data, err := os.ReadFile(got.Attachments[0].Path)
	if err != nil {
		t.Fatalf("read mirrored attachment after retry: %v", err)
	}
	if string(data) != "eventual payload" {
		t.Fatalf("mirrored payload after retry = %q, want eventual payload", data)
	}
}
