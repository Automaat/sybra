//go:build darwin

package main

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

const (
	desktopEmitQueueSize     = 128
	desktopEmitInterval      = 50 * time.Millisecond
	desktopEmitMaxGoroutines = 256
	// desktopEmitBatchSize bounds how many queued events drain per tick. A
	// higher value clears transient backpressure faster; the goroutine gate
	// is re-checked between every emit so a stall can never overshoot the
	// limit by more than one batch.
	desktopEmitBatchSize = 16
	// desktopEmitStallAfter is how long the emit pump must stay gated
	// (goroutines pinned above the limit) before it is treated as a hard
	// stall rather than a transient burst. Reaching the limit requires
	// hundreds of emit goroutines wedged on the Wails main thread, which only
	// happens when that thread stops draining — so a sustained gate is a
	// reliable signal that the webview is frozen.
	desktopEmitStallAfter = 20 * time.Second
)

type desktopEvent struct {
	name string
	data any
}

// desktopEmitter throttles and coalesces Go→frontend events. Each underlying
// Wails Event.Emit spawns a goroutine that blocks on the main UI thread; when
// that thread wedges, those goroutines pile up. The emitter gates emission on
// the live goroutine count to cap the pile-up, and escalates a sustained gate
// to a webview-independent alert so the freeze is never silent.
type desktopEmitter struct {
	emit          func(string, any)
	logger        *slog.Logger
	queue         chan desktopEvent
	interval      time.Duration
	maxGoroutines int
	batchSize     int
	stallAfter    time.Duration

	// now and gated are injectable for testing. gated reports whether the
	// emit pump must hold off because too many goroutines are in flight.
	now   func() time.Time
	gated func() bool

	// onStall / onRecovered fire once per stall episode. They must not block
	// and must not depend on the webview (which is what stalled). Optional.
	onStall     func(time.Duration)
	onRecovered func(time.Duration)

	mu            sync.Mutex
	pending       map[string]desktopEvent
	dropped       int
	lastDropLog   time.Time
	lastPausedLog time.Time

	// pausedSince / stallAlerted are touched only by the run goroutine.
	pausedSince  time.Time
	stallAlerted bool
}

func newDesktopEmitter(ctx context.Context, logger *slog.Logger, emit func(string, any)) *desktopEmitter {
	e := &desktopEmitter{
		emit:          emit,
		logger:        logger,
		queue:         make(chan desktopEvent, desktopEmitQueueSize),
		interval:      desktopEmitInterval,
		maxGoroutines: desktopEmitMaxGoroutines,
		batchSize:     desktopEmitBatchSize,
		stallAfter:    desktopEmitStallAfter,
		pending:       map[string]desktopEvent{},
		now:           time.Now,
	}
	e.gated = func() bool { return runtime.NumGoroutine() > e.maxGoroutines }
	go e.run(ctx)
	return e
}

func (e *desktopEmitter) Emit(name string, data any) {
	ev := desktopEvent{name: name, data: data}
	select {
	case e.queue <- ev:
	default:
		e.coalesce(ev)
	}
}

func (e *desktopEmitter) run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.flush()
		}
	}
}

// flush drains up to batchSize events per tick. It refuses to emit while the
// goroutine gate is tripped, tracking the stall so a sustained one escalates;
// otherwise it announces recovery and drains, re-checking the gate after each
// emit so a burst that crosses the limit stops immediately.
func (e *desktopEmitter) flush() {
	if e.gated() {
		e.notePaused()
		return
	}
	e.noteResumed()

	for range e.batchSize {
		ev, ok := e.next()
		if !ok {
			return
		}
		e.emit(ev.name, ev.data)
		if e.gated() {
			e.notePaused()
			return
		}
	}
}

// next returns the highest-priority pending event: queued events first (FIFO),
// then coalesced events (latest-per-name).
func (e *desktopEmitter) next() (desktopEvent, bool) {
	select {
	case ev := <-e.queue:
		return ev, true
	default:
	}
	return e.takePending()
}

func (e *desktopEmitter) coalesce(ev desktopEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending[ev.name] = ev
	e.dropped++
	now := e.now()
	if now.Sub(e.lastDropLog) >= time.Minute {
		e.logger.Warn("desktop.emit.coalescing", "dropped", e.dropped, "pending", len(e.pending))
		e.lastDropLog = now
		e.dropped = 0
	}
}

func (e *desktopEmitter) takePending() (desktopEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, ev := range e.pending {
		delete(e.pending, name)
		return ev, true
	}
	return desktopEvent{}, false
}

// notePaused records the start of (or continuation of) a gated episode,
// rate-limits the WARN log, and escalates once when the gate has held longer
// than stallAfter.
func (e *desktopEmitter) notePaused() {
	now := e.now()
	if e.pausedSince.IsZero() {
		e.pausedSince = now
	}

	e.mu.Lock()
	queued, pending := len(e.queue), len(e.pending)
	logPaused := now.Sub(e.lastPausedLog) >= time.Minute
	if logPaused {
		e.lastPausedLog = now
	}
	e.mu.Unlock()

	if logPaused {
		e.logger.Warn("desktop.emit.paused", "goroutines", runtime.NumGoroutine(), "limit", e.maxGoroutines, "queued", queued, "pending", pending)
	}

	if !e.stallAlerted && now.Sub(e.pausedSince) >= e.stallAfter {
		e.stallAlerted = true
		stalled := now.Sub(e.pausedSince)
		e.logger.Error("desktop.emit.stalled", "stalled_for", stalled.String(), "goroutines", runtime.NumGoroutine(), "limit", e.maxGoroutines, "queued", queued, "pending", pending)
		if e.onStall != nil {
			e.onStall(stalled)
		}
	}
}

// noteResumed clears the gated episode. If the episode had escalated to a stall
// alert, it announces recovery so the user knows the UI is live again.
func (e *desktopEmitter) noteResumed() {
	if e.pausedSince.IsZero() {
		return
	}
	dur := e.now().Sub(e.pausedSince)
	alerted := e.stallAlerted
	e.pausedSince = time.Time{}
	e.stallAlerted = false
	if alerted {
		e.logger.Info("desktop.emit.recovered", "stalled_for", dur.String())
		if e.onRecovered != nil {
			e.onRecovered(dur)
		}
	}
}
