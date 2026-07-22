package sybra

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Automaat/sybra/internal/httpapi"
)

type lifecycleState uint32

const (
	lifecycleStateIdle lifecycleState = iota
	lifecycleStateRunning
	lifecycleStateDraining
	lifecycleStateStopping
	lifecycleStateStopped
)

type lifecycleHTTPError struct {
	msg    string
	status int
}

func (e lifecycleHTTPError) Error() string   { return e.msg }
func (e lifecycleHTTPError) HTTPStatus() int { return e.status }

func lifecycleBaseContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (a *App) initLifecycle(ctx context.Context) (appCtx, schedulerCtx context.Context) {
	base := lifecycleBaseContext(ctx)
	a.ctx, a.cancel = context.WithCancel(base)
	a.schedulerCtx, a.schedulerCancel = context.WithCancel(a.ctx)
	a.lifecycle.Store(uint32(lifecycleStateRunning))
	return a.ctx, a.schedulerCtx
}

func (a *App) BeginDrain() bool {
	if a == nil {
		return false
	}
	for {
		state := lifecycleState(a.lifecycle.Load())
		switch state {
		case lifecycleStateIdle, lifecycleStateDraining, lifecycleStateStopping, lifecycleStateStopped:
			return false
		case lifecycleStateRunning:
			if !a.lifecycle.CompareAndSwap(uint32(lifecycleStateRunning), uint32(lifecycleStateDraining)) {
				continue
			}
			if a.logger != nil {
				a.logger.Info("app.draining")
			}
			if a.workflowEngine != nil {
				a.workflowEngine.SetAutoDispatch(false)
			}
			if a.schedulerCancel != nil {
				a.schedulerCancel()
			}
			return true
		default:
			return false
		}
	}
}

func (a *App) beginShutdown() {
	if a == nil {
		return
	}
	a.BeginDrain()
	for {
		state := lifecycleState(a.lifecycle.Load())
		switch state {
		case lifecycleStateStopped, lifecycleStateStopping:
			return
		case lifecycleStateIdle:
			if a.lifecycle.CompareAndSwap(uint32(lifecycleStateIdle), uint32(lifecycleStateStopping)) {
				return
			}
		case lifecycleStateRunning, lifecycleStateDraining:
			if a.lifecycle.CompareAndSwap(uint32(state), uint32(lifecycleStateStopping)) {
				if a.cancel != nil {
					a.cancel()
				}
				return
			}
		default:
			return
		}
	}
}

func (a *App) finishShutdown() {
	if a == nil {
		return
	}
	a.lifecycle.Store(uint32(lifecycleStateStopped))
}

func (a *App) lifecycleState() lifecycleState {
	if a == nil {
		return lifecycleStateIdle
	}
	return lifecycleState(a.lifecycle.Load())
}

func (a *App) httpAdmission(service, method string, meta httpapi.MethodMeta) error {
	if meta.ReadOnly {
		return nil
	}
	switch a.lifecycleState() {
	case lifecycleStateDraining, lifecycleStateStopping, lifecycleStateStopped:
		return lifecycleHTTPError{
			msg:    fmt.Sprintf("refusing %s.%s: Sybra is draining for shutdown", service, method),
			status: http.StatusServiceUnavailable,
		}
	default:
		return nil
	}
}
