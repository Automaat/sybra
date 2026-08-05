package monitor

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/provider"
)

// A dispatch refused for want of provider capacity never ran, so it must not
// consume the anomaly's cooldown. canDispatch reserves the window before the
// attempt — necessary so two ticks cannot both fire — which meant a fleet-wide
// rate limit left every anomaly unexamined for a full cooldown afterwards.
func TestCanDispatch_ReleasedWhenNoProviderCapacity(t *testing.T) {
	st := newRunState()
	now := time.Now()
	const fp = "stuck_human_blocked:abc123"
	const cooldown = 30 * time.Minute

	if !st.canDispatch(fp, now, cooldown) {
		t.Fatal("precondition: first dispatch should be allowed")
	}
	if st.canDispatch(fp, now.Add(time.Minute), cooldown) {
		t.Fatal("precondition: the reservation should block a second attempt")
	}

	st.releaseDispatch(fp)

	if !st.canDispatch(fp, now.Add(time.Minute), cooldown) {
		t.Error("after release the anomaly is still on cooldown; the outage was charged to it")
	}
}

type capacityDispatcher struct {
	err   error
	calls int
}

func (d *capacityDispatcher) Dispatchable(Anomaly) (ok bool, skipReason string) { return true, "" }

func (d *capacityDispatcher) Dispatch(context.Context, Anomaly) (string, error) {
	d.calls++
	return "", d.err
}

// The service must distinguish "no capacity" from "this dispatch failed".
func TestDispatchLLMAnomalies_CapacityFailureRetriesNextTick(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantRetried bool
	}{
		{
			name:        "rate-limited provider releases the reservation",
			err:         &provider.UnhealthyError{Provider: "codex", Reason: provider.RateLimitReason, RateLimited: true},
			wantRetried: true,
		},
		{
			// A permanent refusal does not self-heal, so releasing would retry
			// it every tick until a human logs the provider back in.
			name:        "logged-out provider keeps the cooldown",
			err:         &provider.UnhealthyError{Provider: "codex", Reason: "logged_out"},
			wantRetried: false,
		},
		{
			// A real dispatch failure did consume an attempt and must stay on
			// cooldown, or a broken anomaly re-fires every tick.
			name:        "ordinary failure keeps the cooldown",
			err:         errors.New("worktree prepare failed"),
			wantRetried: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &capacityDispatcher{err: tc.err}
			svc := &Service{
				dispatcher: d,
				state:      newRunState(),
				logger:     slog.New(slog.DiscardHandler),
				cfg:        config.MonitorConfig{IssueCooldownMinutes: 30},
			}
			anoms := []Anomaly{{Kind: "stuck_human_blocked", TaskID: "t1", Fingerprint: "fp1", RequiresLLM: true}}

			svc.dispatchLLMAnomalies(context.Background(), time.Now(), anoms)
			svc.dispatchLLMAnomalies(context.Background(), time.Now().Add(time.Minute), anoms)

			wantCalls := 1
			if tc.wantRetried {
				wantCalls = 2
			}
			if d.calls != wantCalls {
				t.Errorf("dispatch attempts = %d, want %d", d.calls, wantCalls)
			}
		})
	}
}
