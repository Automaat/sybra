package agentorch

import (
	"context"
	"testing"
)

func TestBaseCtxFallsBackToBackground(t *testing.T) {
	o := &Orchestrator{}
	if o.baseCtx() != context.Background() {
		t.Fatal("nil app ctx must fall back to context.Background()")
	}
}

func TestBaseCtxPropagatesCancel(t *testing.T) {
	o := &Orchestrator{}
	ctx, cancel := context.WithCancel(context.Background())
	o.SetContext(ctx)
	if o.baseCtx() != ctx {
		t.Fatal("baseCtx must return the set app ctx")
	}
	cancel()
	if o.baseCtx().Err() == nil {
		t.Fatal("cancelling the app ctx must propagate through baseCtx (worktree prep aborts on shutdown)")
	}
}
