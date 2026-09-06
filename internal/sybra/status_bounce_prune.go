package sybra

import (
	"context"
	"time"
)

// Pruning runs outside task mutation observers. The global cutoff preserves
// the late-delivery fence even after an idle task's episode watermark is freed.
func (a *App) startStatusBouncePruning(ctx context.Context) {
	a.wg.Go(func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				a.pruneStatusBounces(now)
			}
		}
	})
}

func (a *App) pruneStatusBounces(now time.Time) {
	cutoff := now.Add(-statusBounceWindow)
	a.statusBounceMu.Lock()
	defer a.statusBounceMu.Unlock()
	if cutoff.Before(a.statusBounceCutoff) {
		return
	}
	a.statusBounceCutoff = cutoff
	for id, state := range a.statusBounces {
		if !state.lastSeen.After(cutoff) {
			delete(a.statusBounces, id)
		}
	}
}
