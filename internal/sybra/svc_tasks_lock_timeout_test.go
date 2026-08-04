package sybra

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func assertRetryableTaskLockError(t *testing.T, err error, lockPath string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected lock timeout error")
	}
	var ce interface{ HTTPStatus() int }
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want client-facing unavailable error", err)
	}
	if ce.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("HTTPStatus = %d, want %d", ce.HTTPStatus(), http.StatusServiceUnavailable)
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("error %q missing lock path %q", err, lockPath)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("error %q missing holder pid %d", err, os.Getpid())
	}
}

func TestTaskService_UpdateTask_LockTimeoutReturnsUnavailable(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	created, err := svc.tasks.Create("locked update", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(created.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()

	_, err = svc.UpdateTask(created.ID, map[string]any{"title": "blocked"})
	assertRetryableTaskLockError(t, err, created.FilePath+".lock")
}

func TestTaskService_AssignTask_LockTimeoutReturnsUnavailable(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	created, err := svc.tasks.Create("locked assign", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(created.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()

	err = svc.AssignTask(task.Task{
		ID:        created.ID,
		Title:     "changed",
		Status:    task.StatusTodo,
		AgentMode: task.AgentModeHeadless,
		CreatedAt: created.CreatedAt,
	})
	assertRetryableTaskLockError(t, err, created.FilePath+".lock")
}

func TestTaskService_BlessTampering_LockTimeoutReturnsUnavailable(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	created, err := svc.tasks.Create("locked bless", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := svc.tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(workflow.TamperFlaggedReasonPrefix + " changed test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(flagged.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()

	_, err = svc.BlessTampering(flagged.ID)
	assertRetryableTaskLockError(t, err, flagged.FilePath+".lock")
}

func TestTaskService_DispatchFromHumanRequired_LockTimeoutReturnsUnavailable(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	created, err := svc.tasks.Create("locked dispatch", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	human, err := svc.tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("retry"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.LockFile(human.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()

	_, err = svc.DispatchFromHumanRequired(human.ID, string(task.StatusDone), "resume")
	assertRetryableTaskLockError(t, err, human.FilePath+".lock")
}
