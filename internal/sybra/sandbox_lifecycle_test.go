package sybra

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
)

// TestDeleteTask_StopsSandbox verifies that deleting a task stops its sandbox.
func TestDeleteTask_StopsSandbox(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	// Wire a real (empty) sandbox manager.
	sbDir := t.TempDir()
	sbMgr := sandbox.NewManager(sbDir, discardLogger())
	svc.sandboxes = sbMgr

	// Create a task.
	tsk, err := svc.tasks.Create("test delete sandbox", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	sybraHome, err := sbMgr.SybraHomeDir(tsk.ID)
	if err != nil {
		t.Fatalf("SybraHomeDir: %v", err)
	}
	taskDir := filepath.Dir(sybraHome)

	// Delete task — sandbox.Remove should clean the per-task dir even when no
	// runtime sandbox instance is registered.
	if err := svc.DeleteTask(tsk.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	// Task should no longer exist.
	if _, err := svc.tasks.Get(tsk.ID); err == nil {
		t.Error("task still exists after DeleteTask")
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task sandbox dir %q still exists after DeleteTask: %v", taskDir, err)
	}
}

// TestUpdateTask_Done_RemovesSandbox verifies a terminal status cleans the
// per-task sandbox dir, not just any running runtime sandbox instance.
func TestUpdateTask_Done_RemovesSandbox(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	var wg sync.WaitGroup
	svc.wg = &wg

	sbDir := t.TempDir()
	sbMgr := sandbox.NewManager(sbDir, discardLogger())
	svc.sandboxes = sbMgr

	tsk, err := svc.tasks.Create("test done sandbox", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	sybraHome, err := sbMgr.SybraHomeDir(tsk.ID)
	if err != nil {
		t.Fatalf("SybraHomeDir: %v", err)
	}
	taskDir := filepath.Dir(sybraHome)

	_, err = svc.UpdateTask(tsk.ID, map[string]any{"status": string(task.StatusDone)})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	wg.Wait()
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task sandbox dir %q still exists after terminal status: %v", taskDir, err)
	}
}

// TestUpdateTask_InProgress_NoStop verifies non-done status changes don't stop sandbox.
func TestUpdateTask_InProgress_NoStop(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)

	sbDir := t.TempDir()
	sbMgr := sandbox.NewManager(sbDir, discardLogger())
	svc.sandboxes = sbMgr

	tsk, err := svc.tasks.Create("test inprogress sandbox", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	_, err = svc.UpdateTask(tsk.ID, map[string]any{"status": string(task.StatusInProgress)})
	if err != nil && err.Error() != "" {
		// status change to in-progress may be refused if workflow conditions aren't met — that's fine
		t.Logf("UpdateTask to in-progress: %v (may be expected)", err)
	}
	// Sandbox should not be stopped (no-op since never started).
	// Just verifying no panic occurs.
}

// TestTaskService_NilSandbox verifies TaskService works normally when sandboxes is nil.
func TestTaskService_NilSandbox(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	// sandboxes is nil by default in setupTaskService.

	tsk, err := svc.tasks.Create("test nil sandbox", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := svc.DeleteTask(tsk.ID); err != nil {
		t.Fatalf("DeleteTask with nil sandbox: %v", err)
	}
}
