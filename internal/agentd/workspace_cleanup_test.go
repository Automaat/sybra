package agentd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agentworkspace"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestPrepareRunWorkspaceReclaimsOnlyUnprotectedRunsOnDiskPressure(t *testing.T) {
	root := t.TempDir()
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"orphan", "active", "artifact"} {
		if err := os.Mkdir(filepath.Join(root, runID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.update(func(state *durableState) error {
		state.RunAgents["active"] = "agent-active"
		state.Artifacts["manifest-artifact"] = workercontrol.ArtifactUpload{Manifest: executioncontract.ArtifactManifest{RunID: "artifact"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	daemon := &Daemon{
		cfg: Config{WorkspaceRoot: root}, logger: slog.New(slog.DiscardHandler), spool: spool,
		prepareWorkspace: func(_ context.Context, workspaceRoot, _ string, spec executioncontract.RunSpec) (agentworkspace.Layout, error) {
			calls++
			if calls <= 2 {
				return agentworkspace.Layout{}, errors.New("git clone: fatal: write error: Disk quota exceeded")
			}
			return agentworkspace.Layout{RunRoot: filepath.Join(workspaceRoot, spec.RunID)}, nil
		},
	}
	layout, release, err := daemon.prepareRunWorkspace(t.Context(), "source", executioncontract.RunSpec{RunID: "new-run"})
	release()
	if err != nil {
		t.Fatalf("prepare after pressure reclaim: %v", err)
	}
	if calls != 3 || layout.RunRoot != filepath.Join(root, "new-run") {
		t.Fatalf("prepare calls/layout = %d/%+v", calls, layout)
	}
	if _, err := os.Stat(filepath.Join(root, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprotected workspace remains: %v", err)
	}
	for _, runID := range []string{"active", "artifact"} {
		if _, err := os.Stat(filepath.Join(root, runID)); err != nil {
			t.Fatalf("protected workspace %s removed: %v", runID, err)
		}
	}
}

func TestPrepareRunWorkspaceDoesNotReclaimForNonPressureFailure(t *testing.T) {
	root := t.TempDir()
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, "orphan")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	daemon := &Daemon{
		cfg: Config{WorkspaceRoot: root}, logger: slog.New(slog.DiscardHandler), spool: spool,
		prepareWorkspace: func(context.Context, string, string, executioncontract.RunSpec) (agentworkspace.Layout, error) {
			calls++
			return agentworkspace.Layout{}, errors.New("immutable base is unavailable")
		},
	}
	_, release, err := daemon.prepareRunWorkspace(t.Context(), "source", executioncontract.RunSpec{RunID: "new-run"})
	release()
	if err == nil || calls != 1 {
		t.Fatalf("non-pressure prepare error/calls = %v/%d", err, calls)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("non-pressure failure removed diagnostic workspace: %v", err)
	}
}

func TestPrepareRunWorkspaceAllowsConcurrentOrdinaryStarts(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 2)
	unblock := make(chan struct{})
	daemon := &Daemon{
		cfg: Config{WorkspaceRoot: t.TempDir()}, logger: slog.New(slog.DiscardHandler), spool: spool,
		prepareWorkspace: func(_ context.Context, _ string, _ string, spec executioncontract.RunSpec) (agentworkspace.Layout, error) {
			entered <- spec.RunID
			<-unblock
			return agentworkspace.Layout{RunRoot: spec.RunID}, nil
		},
	}
	done := make(chan error, 2)
	for _, runID := range []string{"run-a", "run-b"} {
		go func() {
			_, release, err := daemon.prepareRunWorkspace(t.Context(), "source", executioncontract.RunSpec{RunID: runID})
			release()
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("ordinary workspace preparations were serialized")
		}
	}
	close(unblock)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceRetentionPruneDoesNotBlockBehindPreparation(t *testing.T) {
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{
		cfg:    Config{WorkspaceRoot: t.TempDir(), WorkspaceRetentionHours: 168},
		logger: slog.New(slog.DiscardHandler), spool: spool,
	}
	daemon.workspaceMu.RLock()
	done := make(chan struct{})
	go func() {
		daemon.pruneExpiredWorkspaces()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention pruning blocked heartbeat behind workspace preparation")
	}
	daemon.workspaceMu.RUnlock()
}

func TestAckArtifactRemovesDurableEntryAndWorkspace(t *testing.T) {
	root := t.TempDir()
	spool, err := OpenSpool(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(root, "run")
	if err := os.Mkdir(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spool.QueueArtifact(workercontrol.ArtifactUpload{Manifest: executioncontract.ArtifactManifest{ManifestID: "manifest", RunID: "run"}}); err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{cfg: Config{WorkspaceRoot: root}, spool: spool}
	if err := daemon.ackArtifactAndRemoveWorkspace("manifest", "run"); err != nil {
		t.Fatal(err)
	}
	if len(spool.snapshot().Artifacts) != 0 {
		t.Fatal("acknowledged artifact remains durable")
	}
	if _, err := os.Stat(runRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged workspace remains: %v", err)
	}
}

func TestWorkspacePressureErrorRecognizesGitAndFilesystemFailures(t *testing.T) {
	for _, err := range []error{
		errors.New("fatal: write error: Disk quota exceeded"),
		errors.New("clone: no space left on device"),
		syscall.ENOSPC,
		syscall.EDQUOT,
	} {
		if !isWorkspacePressureError(err) {
			t.Fatalf("pressure error not recognized: %v", err)
		}
	}
	if isWorkspacePressureError(errors.New("immutable base is unavailable")) {
		t.Fatal("ordinary workspace error classified as pressure")
	}
}
