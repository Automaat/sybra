package watcher

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/fsnotify/fsnotify"
)

type EmitFunc func(event string, data any)

type Watcher struct {
	dir    string
	emit   EmitFunc
	logger *slog.Logger
	ready  chan struct{}
	done   chan struct{}
}

func New(dir string, emit EmitFunc, logger *slog.Logger) *Watcher {
	return &Watcher{
		dir:    dir,
		emit:   emit,
		logger: logger,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Ready returns a channel closed when the watcher loop is running.
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// Done returns a channel closed when the watcher loop exits.
func (w *Watcher) Done() <-chan struct{} { return w.done }

func (w *Watcher) Start(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := fw.Add(w.dir); err != nil {
		_ = fw.Close()
		return err
	}

	go w.loop(ctx, fw)
	return nil
}

func (w *Watcher) loop(ctx context.Context, fw *fsnotify.Watcher) {
	defer func() {
		_ = fw.Close()
		close(w.done)
	}()
	close(w.ready)

	const debounceInterval = 200 * time.Millisecond

	// Trailing-edge debounce: coalesce bursts of events for the same file
	// and emit a single event after the burst settles. The previous
	// leading-edge implementation silently dropped the last write in a
	// burst, leaving consumers with stale content.
	pending := make(map[string]fsnotify.Op)
	timers := make(map[string]*time.Timer)
	flushCh := make(chan string, 64)

	stopAllTimers := func() {
		for _, t := range timers {
			t.Stop()
		}
	}

	for {
		select {
		case <-ctx.Done():
			stopAllTimers()
			return

		case name := <-flushCh:
			op, ok := pending[name]
			if !ok {
				continue
			}
			delete(pending, name)
			delete(timers, name)
			w.emitFor(name, op)

		case event, ok := <-fw.Events:
			if !ok {
				stopAllTimers()
				return
			}
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}
			// OR ops so a Create+Write burst still surfaces as Create.
			pending[event.Name] |= event.Op
			if t, exists := timers[event.Name]; exists {
				t.Stop()
			}
			name := event.Name
			timers[name] = time.AfterFunc(debounceInterval, func() {
				select {
				case flushCh <- name:
				case <-ctx.Done():
				}
			})

		case err, ok := <-fw.Errors:
			if !ok {
				stopAllTimers()
				return
			}
			w.logger.Error("watcher.error", "err", err)
		}
	}
}

func (w *Watcher) emitFor(name string, op fsnotify.Op) {
	switch {
	case op.Has(fsnotify.Create):
		w.logger.Info("watcher.event", "op", "created", "file", name)
		w.emit(events.TaskCreated, name)
	case op.Has(fsnotify.Write):
		w.logger.Debug("watcher.event", "op", "updated", "file", name)
		w.emit(events.TaskUpdated, name)
	case op.Has(fsnotify.Remove):
		// Atomic writes (tmp+rename) emit Remove for the old inode.
		// If the file still exists, treat as update instead of delete.
		if _, err := os.Stat(name); err == nil {
			w.logger.Debug("watcher.event", "op", "updated", "file", name)
			w.emit(events.TaskUpdated, name)
		} else {
			w.logger.Info("watcher.event", "op", "deleted", "file", name)
			w.emit(events.TaskDeleted, name)
		}
	}
}
