package sybra

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/httpapi"
)

func TestWaitGroupContext(t *testing.T) {
	tests := []struct {
		name  string
		block bool
		grace time.Duration
		want  bool
	}{
		{name: "completes within grace", block: false, grace: time.Second, want: true},
		{name: "times out when a goroutine never finishes", block: true, grace: 10 * time.Millisecond, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(1)
			if !tt.block {
				go wg.Done()
			}
			ctx, cancel := context.WithTimeout(context.Background(), tt.grace)
			defer cancel()
			if got := waitGroupContext(ctx, &wg); got != tt.want {
				t.Fatalf("waitGroupContext = %v, want %v", got, tt.want)
			}
			if tt.block {
				wg.Done()
			}
		})
	}
}

func TestShutdownWaitContext(t *testing.T) {
	t.Run("adds fallback deadline when caller has none", func(t *testing.T) {
		ctx, cancel := shutdownWaitContext(context.Background())
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Deadline() = false, want fallback deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > appShutdownWaitGrace {
			t.Fatalf("remaining = %s, want within (0,%s]", remaining, appShutdownWaitGrace)
		}
	})

	t.Run("preserves caller deadline", func(t *testing.T) {
		parent, cancelParent := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancelParent()

		ctx, cancel := shutdownWaitContext(parent)
		defer cancel()

		parentDeadline, ok := parent.Deadline()
		if !ok {
			t.Fatal("parent Deadline() = false, want true")
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Deadline() = false, want caller deadline")
		}
		if !deadline.Equal(parentDeadline) {
			t.Fatalf("deadline = %v, want %v", deadline, parentDeadline)
		}
	})
}

func TestBeginDrainCancelsSchedulerOnly(t *testing.T) {
	a := &App{}
	a.initLifecycle(context.Background())

	if !a.BeginDrain() {
		t.Fatal("BeginDrain = false, want true")
	}

	select {
	case <-a.schedulerCtx.Done():
	default:
		t.Fatal("schedulerCtx not canceled by BeginDrain")
	}
	select {
	case <-a.ctx.Done():
		t.Fatal("app ctx canceled during drain; want accepted work to stay alive")
	default:
	}

	if err := a.HTTPAdmission("App", "ListBackgroundOps", httpapi.MethodMeta{ReadOnly: true}); err != nil {
		t.Fatalf("read-only admission error = %v, want nil", err)
	}
	err := a.HTTPAdmission("App", "SetDesktopNotifications", httpapi.MethodMeta{})
	if err == nil {
		t.Fatal("mutating admission error = nil, want service unavailable")
	}
	var clientErr interface {
		error
		HTTPStatus() int
	}
	if !errors.As(err, &clientErr) {
		t.Fatalf("mutating admission error type = %T, want ClientError", err)
	}
	if clientErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("mutating admission status = %d, want 503", clientErr.HTTPStatus())
	}
}

func TestBeginDrainRejectsLateTrackedBackgroundWork(t *testing.T) {
	a := &App{}
	a.initLifecycle(context.Background())
	if !a.BeginDrain() {
		t.Fatal("BeginDrain = false, want true")
	}

	ran := make(chan struct{}, 1)
	if a.goWhileRunning(func() { ran <- struct{}{} }) {
		t.Fatal("goWhileRunning admitted work after drain")
	}
	select {
	case <-ran:
		t.Fatal("late background work ran after drain")
	default:
	}
}

func TestBeginShutdownCancelsAcceptedWork(t *testing.T) {
	a := &App{logger: discardLogger()}
	a.initLifecycle(context.Background())
	a.beginShutdown()

	select {
	case <-a.ctx.Done():
	default:
		t.Fatal("app ctx not canceled by beginShutdown")
	}
	if got := a.lifecycleState(); got != lifecycleStateStopping {
		t.Fatalf("lifecycle state = %v, want stopping", got)
	}
}

func TestShutdownDrainsAcceptedWorkBeforeCancel(t *testing.T) {
	a := &App{logger: discardLogger()}
	a.initLifecycle(context.Background())

	release := make(chan struct{})
	a.wg.Go(func() {
		<-release
	})

	shutdownDone := make(chan struct{})
	go func() {
		a.Shutdown(context.Background())
		close(shutdownDone)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a.lifecycleState() == lifecycleStateDraining {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.lifecycleState(); got != lifecycleStateDraining {
		t.Fatalf("lifecycle state during wait = %v, want draining", got)
	}
	select {
	case <-a.schedulerCtx.Done():
	default:
		t.Fatal("schedulerCtx not canceled during shutdown drain")
	}
	select {
	case <-a.ctx.Done():
		t.Fatal("app ctx canceled before accepted work drained")
	default:
	}
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before accepted work completed")
	default:
	}

	close(release)

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not complete after accepted work drained")
	}
	select {
	case <-a.ctx.Done():
	default:
		t.Fatal("app ctx not canceled after shutdown moved to stopping")
	}
}

func TestTaskWatcherStaysAliveUntilAgentShutdownFinishes(t *testing.T) {
	taskEvents := make(chan string, 1)
	a := &App{
		tasksDir: t.TempDir(),
		logger:   discardLogger(),
		emit:     func(string, any) {},
	}
	a.initLifecycle(context.Background())
	a.initFileWatcher(a.watcherCtx, func(event string, _ any) {
		select {
		case taskEvents <- event:
		default:
		}
	})
	t.Cleanup(a.stopFileWatcher)

	select {
	case <-a.watcher.Ready():
	case <-time.After(time.Second):
		t.Fatal("task watcher did not become ready")
	}

	a.BeginDrain()
	a.beginShutdown()

	select {
	case <-a.ctx.Done():
	default:
		t.Fatal("app ctx not canceled by beginShutdown")
	}
	select {
	case <-a.watcher.Done():
		t.Fatal("task watcher stopped before agent shutdown finished")
	default:
	}

	if err := os.WriteFile(a.tasksDir+"/during-shutdown.md", []byte("---\nid: during-shutdown\nstatus: done\n---\n"), 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	select {
	case got := <-taskEvents:
		if got != events.TaskCreated && got != events.TaskUpdated {
			t.Fatalf("watcher event = %q, want task create/update", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task watcher did not emit after beginShutdown")
	}

	a.stopFileWatcher()
	select {
	case <-a.watcher.Done():
	case <-time.After(time.Second):
		t.Fatal("task watcher did not stop after explicit shutdown")
	}
}
