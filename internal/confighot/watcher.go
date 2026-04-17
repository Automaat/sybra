package confighot

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a single config file and calls onChange after each
// debounced change burst. The parent directory is watched rather than the
// file itself so that atomic editor saves (write-tmp + rename) are caught.
type Watcher struct {
	path     string
	onChange func()
	logger   *slog.Logger
	ready    chan struct{}
	done     chan struct{}
}

// New creates a Watcher for path. onChange is called after each debounced
// burst of Write/Create/Rename events on that file.
func New(path string, onChange func(), logger *slog.Logger) *Watcher {
	return &Watcher{
		path:     path,
		onChange: onChange,
		logger:   logger,
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Ready returns a channel closed when the watch loop is running.
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// Done returns a channel closed when the watch loop exits.
func (w *Watcher) Done() <-chan struct{} { return w.done }

// Start begins watching. The watcher stops when ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(w.path)
	if err := fw.Add(dir); err != nil {
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

	const debounceInterval = 500 * time.Millisecond

	target := filepath.Base(w.path)
	var deadline time.Time
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	var timerCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return

		case event, ok := <-fw.Events:
			if !ok {
				stopTimer(timer)
				return
			}
			if filepath.Base(event.Name) != target {
				continue
			}
			if !event.Op.Has(fsnotify.Write) && !event.Op.Has(fsnotify.Create) && !event.Op.Has(fsnotify.Rename) {
				continue
			}
			deadline = time.Now().Add(debounceInterval)
			timerCh = resetTimer(timer, deadline)

		case <-timerCh:
			timerCh = nil
			w.logger.Info("confighot.changed", "file", w.path)
			w.onChange()

		case err, ok := <-fw.Errors:
			if !ok {
				stopTimer(timer)
				return
			}
			w.logger.Error("confighot.error", "err", err)
		}
	}
}

func resetTimer(timer *time.Timer, deadline time.Time) <-chan time.Time {
	wait := max(time.Until(deadline), 0)
	stopTimer(timer)
	timer.Reset(wait)
	return timer.C
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
