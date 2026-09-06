package agent

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExecutionCompletionKeepsWinningReceiptAndOutcome(t *testing.T) {
	for _, firstReceipt := range []string{"v1:terminal", ""} {
		t.Run(map[bool]string{true: "terminal wins", false: "observer wins"}[firstReceipt != ""], func(t *testing.T) {
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			firstErr, laterErr := errors.New("winning outcome"), errors.New("late outcome")
			m, _ := newTestManager(t, ManagerConfig{OnComplete: func(a *Agent) {
				close(entered)
				<-release
				if got := a.GetRemoteCompletionReceipt(); got != firstReceipt {
					t.Errorf("callback receipt = %q, want %q", got, firstReceipt)
				}
				if got := a.GetExitErr(); !errors.Is(got, firstErr) {
					t.Errorf("callback outcome = %v, want winning outcome", got)
				}
			}})
			m.SetExecutionBackend(newSinkDrivenFakeBackend())
			a, err := m.Run(RunConfig{Mode: "headless", Dir: t.TempDir(), Prompt: "not executed"})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				m.emitExecutionEvent(t.Context(), "race", ExecutionEvent{Kind: ExecutionCompleted, Err: firstErr, RemoteCompletionReceipt: firstReceipt}, a, 0, nil)
				close(finished)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("winning completion did not enter callback")
			}
			laterReceipt := "v1:terminal"
			if firstReceipt != "" {
				laterReceipt = ""
			}
			// Model another terminal emitter that obtained the Agent before the
			// first emitter unregistered it. It must not mutate callback inputs.
			lateFinished := make(chan struct{})
			go func() {
				m.emitExecutionEvent(t.Context(), "race", ExecutionEvent{Kind: ExecutionCompleted, Err: laterErr, RemoteCompletionReceipt: laterReceipt, PermanentFailure: true}, a, 0, nil)
				close(lateFinished)
			}()
			select {
			case <-lateFinished:
			case <-time.After(time.Second):
				t.Fatal("duplicate terminal emitter blocked behind the completion callback")
			}
			if a.GetEscalationReason() != "" {
				t.Fatal("late terminal changed escalation metadata")
			}
			unblock()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("completion did not finish")
			}
		})
	}
}
