package sybra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/pressure"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestAgentAdapterUsesRemotePressurePostureBeforePlacement(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	now := time.Now().UTC()
	put := func(id, node string, wf *workflow.Execution) {
		t.Helper()
		if _, err := store.Put(task.Task{
			ID: id, Title: id, ProjectID: "owner/repo", NodeOverride: node,
			Status: task.StatusInProgress, CreatedAt: now, UpdatedAt: now, Workflow: wf,
		}); err != nil {
			t.Fatal(err)
		}
	}
	remoteWorkflow := &workflow.Execution{WorkflowID: "ship", CurrentStep: "implement", State: workflow.ExecWaiting}
	put("remote", "", remoteWorkflow)
	put("forced-local", "local", remoteWorkflow)
	put("no-effect", "", nil)

	gate := pressure.New(config.PressureConfig{
		Enabled: true, MinDiskFreePercent: 101, RemoteMinDiskFreeBytes: 1, SampleIntervalSeconds: 60,
	}, t.TempDir(), discardLogger())
	adapter := &agentAdapter{
		tasks: tasks, pressure: gate, remotePlacement: true,
		agentOrch: agentorch.New(tasks, nil, nil, nil, discardLogger(), nil, &config.Config{}),
	}

	if ok, reason := adapter.AdmitDispatch("remote", "implementation", "headless"); !ok || reason != "" {
		t.Fatalf("remote AdmitDispatch() = (%v, %q), want admitted despite local percentage pressure", ok, reason)
	}
	if ok, reason := adapter.AdmitDispatch("forced-local", "implementation", "headless"); ok || reason == "" {
		t.Fatalf("forced-local AdmitDispatch() = (%v, %q), want local pressure denial", ok, reason)
	}
	if ok, reason := adapter.AdmitDispatch("no-effect", "implementation", "headless"); ok || reason == "" {
		t.Fatalf("no-effect AdmitDispatch() = (%v, %q), want local pressure denial", ok, reason)
	}
	adapter.remotePlacement = false
	if ok, reason := adapter.AdmitDispatch("remote", "implementation", "headless"); ok || reason == "" {
		t.Fatalf("standalone remote-shaped AdmitDispatch() = (%v, %q), want local pressure denial", ok, reason)
	}
}
