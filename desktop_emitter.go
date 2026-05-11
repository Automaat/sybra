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
)

type desktopEvent struct {
	name string
	data any
}

type desktopEmitter struct {
	emit          func(string, any)
	logger        *slog.Logger
	queue         chan desktopEvent
	interval      time.Duration
	maxGoroutines int

	mu            sync.Mutex
	pending       map[string]desktopEvent
	dropped       int
	lastDropLog   time.Time
	lastPausedLog time.Time
}

func newDesktopEmitter(ctx context.Context, logger *slog.Logger, emit func(string, any)) *desktopEmitter {
	e := &desktopEmitter{
		emit:          emit,
		logger:        logger,
		queue:         make(chan desktopEvent, desktopEmitQueueSize),
		interval:      desktopEmitInterval,
		maxGoroutines: desktopEmitMaxGoroutines,
		pending:       map[string]desktopEvent{},
	}
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
			e.flushOne()
		}
	}
}

func (e *desktopEmitter) flushOne() {
	if runtime.NumGoroutine() > e.maxGoroutines {
		e.logPaused()
		return
	}

	select {
	case ev := <-e.queue:
		e.emit(ev.name, ev.data)
		return
	default:
	}

	if ev, ok := e.takePending(); ok {
		e.emit(ev.name, ev.data)
	}
}

func (e *desktopEmitter) coalesce(ev desktopEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending[ev.name] = ev
	e.dropped++
	now := time.Now()
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

func (e *desktopEmitter) logPaused() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if now.Sub(e.lastPausedLog) < time.Minute {
		return
	}
	e.logger.Warn("desktop.emit.paused", "goroutines", runtime.NumGoroutine(), "limit", e.maxGoroutines, "queued", len(e.queue), "pending", len(e.pending))
	e.lastPausedLog = now
}
