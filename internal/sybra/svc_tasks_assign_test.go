package sybra

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestAssignTaskPersistsVerbatim(t *testing.T) {
	svc, _ := setupTaskService(t)

	pushed := task.Task{
		ID:           "leader-01",
		Title:        "Implement the thing",
		Status:       task.StatusTodo,
		AgentMode:    task.AgentModeHeadless,
		ProjectID:    "owner/repo",
		AssignedNode: "pet-box",
		Body:         "## Description\nfrom the leader",
	}
	if err := svc.AssignTask(pushed); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	got, err := svc.GetTask("leader-01")
	if err != nil {
		t.Fatalf("GetTask after assign: %v", err)
	}
	if got.ID != "leader-01" || got.Title != "Implement the thing" {
		t.Fatalf("mirror did not preserve identity: %+v", got)
	}
	if got.Status != task.StatusTodo || got.AssignedNode != "pet-box" || got.ProjectID != "owner/repo" {
		t.Fatalf("mirror did not preserve fields: %+v", got)
	}
	if got.Body != "## Description\nfrom the leader" {
		t.Fatalf("mirror did not preserve body: %q", got.Body)
	}
}

func TestAssignTaskUpsertsOnRepeatedPush(t *testing.T) {
	svc, _ := setupTaskService(t)

	base := task.Task{ID: "leader-02", Title: "t", Status: task.StatusTodo, AgentMode: task.AgentModeHeadless}
	if err := svc.AssignTask(base); err != nil {
		t.Fatal(err)
	}
	base.Status = task.StatusInProgress
	if err := svc.AssignTask(base); err != nil {
		t.Fatalf("second push (status update) should upsert: %v", err)
	}
	got, err := svc.GetTask("leader-02")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("upsert did not apply the new status: %s", got.Status)
	}

	all, err := svc.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tk := range all {
		if tk.ID == "leader-02" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("upsert produced %d copies of leader-02, want 1", count)
	}
}

func TestAssignTaskIdenticalPushIsNoOp(t *testing.T) {
	svc, _ := setupTaskService(t)

	var (
		mu    sync.Mutex
		fired []string
	)
	svc.tasks.SetStatusChangeHook(func(_, _, to string) {
		mu.Lock()
		fired = append(fired, to)
		mu.Unlock()
	})

	pushed := task.Task{
		ID:           "leader-noop",
		Title:        "t",
		Status:       task.StatusTodo,
		AgentMode:    task.AgentModeHeadless,
		ProjectID:    "owner/repo",
		AssignedNode: "pet-box",
	}
	if err := svc.AssignTask(pushed); err != nil {
		t.Fatalf("first AssignTask: %v", err)
	}
	got, err := svc.GetTask("leader-noop")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	infoBefore, err := os.Stat(got.FilePath)
	if err != nil {
		t.Fatalf("Stat before second push: %v", err)
	}
	bodyBefore, err := os.ReadFile(got.FilePath)
	if err != nil {
		t.Fatalf("ReadFile before second push: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := svc.AssignTask(pushed); err != nil {
		t.Fatalf("second identical AssignTask: %v", err)
	}
	infoAfter, err := os.Stat(got.FilePath)
	if err != nil {
		t.Fatalf("Stat after second push: %v", err)
	}
	bodyAfter, err := os.ReadFile(got.FilePath)
	if err != nil {
		t.Fatalf("ReadFile after second push: %v", err)
	}

	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatalf("identical push rewrote task file: modtime %v -> %v", infoBefore.ModTime(), infoAfter.ModTime())
	}
	if !bytes.Equal(bodyBefore, bodyAfter) {
		t.Fatal("identical push rewrote task contents")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != string(task.StatusTodo) {
		t.Fatalf("status hook fired %v, want one create-time todo transition", fired)
	}
}

func TestAssignTaskDispatchesStageStatus(t *testing.T) {
	svc, _ := setupTaskService(t)

	var mu sync.Mutex
	var fired []string
	svc.tasks.SetStatusChangeHook(func(_, _, to string) {
		mu.Lock()
		fired = append(fired, to)
		mu.Unlock()
	})

	pushed := task.Task{
		ID:        "leader-stage",
		Title:     "mid-workflow handoff",
		Status:    task.StatusReadyReview,
		AgentMode: task.AgentModeHeadless,
	}
	if err := svc.AssignTask(pushed); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != string(task.StatusReadyReview) {
		t.Fatalf("status hook fired %v, want [ready-review] — otherwise the pushed stage never dispatches", fired)
	}
}

func TestAssignTaskRejectsMalformed(t *testing.T) {
	svc, _ := setupTaskService(t)

	cases := []struct {
		name string
		in   task.Task
	}{
		{"no id", task.Task{Title: "t", Status: task.StatusTodo}},
		{"path traversal id", task.Task{ID: "../../etc/passwd", Title: "t", Status: task.StatusTodo}},
		{"slash in id", task.Task{ID: "a/b", Title: "t", Status: task.StatusTodo}},
		{"dotdot id", task.Task{ID: "..", Title: "t", Status: task.StatusTodo}},
		{"no title", task.Task{ID: "x", Status: task.StatusTodo}},
		{"bad status", task.Task{ID: "x", Title: "t", Status: task.Status("bogus")}},
		{"bad agent mode", task.Task{ID: "x", Title: "t", Status: task.StatusTodo, AgentMode: "telepathy"}},
		{"bad task type", task.Task{ID: "x", Title: "t", Status: task.StatusTodo, TaskType: task.TaskType("weird")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := svc.AssignTask(c.in)
			if err == nil {
				t.Fatal("want validation error")
			}
			var ce *clientError
			if !errors.As(err, &ce) || ce.HTTPStatus() != 400 {
				t.Fatalf("want 400 clientError, got %v", err)
			}
			if _, getErr := svc.GetTask("x"); getErr == nil {
				t.Fatal("a rejected task must not have been written to the store")
			}
		})
	}
}
