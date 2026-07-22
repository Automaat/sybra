package sybra

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/httpapi"
)

func TestWaitGroupTimeout(t *testing.T) {
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
			if got := waitGroupTimeout(&wg, tt.grace); got != tt.want {
				t.Fatalf("waitGroupTimeout = %v, want %v", got, tt.want)
			}
			if tt.block {
				wg.Done()
			}
		})
	}
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

func TestBeginShutdownCancelsAcceptedWork(t *testing.T) {
	a := &App{}
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
