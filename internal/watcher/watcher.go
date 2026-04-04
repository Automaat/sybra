package watcher

import (
	"context"
	"log/slog"
	"strings"
	"time"

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

	debounce := make(map[string]time.Time)
	const debounceInterval = 200 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fw.Events:
			if !ok {
				return
			}
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}

			now := time.Now()
			if last, exists := debounce[event.Name]; exists && now.Sub(last) < debounceInterval {
				continue
			}
			debounce[event.Name] = now

			switch {
			case event.Has(fsnotify.Create):
				w.logger.Info("watcher.event", "op", "created", "file", event.Name)
				w.emit("task:created", event.Name)
			case event.Has(fsnotify.Write):
				w.logger.Debug("watcher.event", "op", "updated", "file", event.Name)
				w.emit("task:updated", event.Name)
			case event.Has(fsnotify.Remove):
				w.logger.Info("watcher.event", "op", "deleted", "file", event.Name)
				w.emit("task:deleted", event.Name)
			}

		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			w.logger.Error("watcher.error", "err", err)
		}
	}
}
