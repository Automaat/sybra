package watcher

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/fsnotify/fsnotify"
)

type EmitFunc func(event string, data any)

// defaultReconcileInterval is how often the poll-based reconciliation pass
// re-scans the directory. It is a backstop for fsnotify's per-file OS-level
// watch state (e.g. kqueue on darwin opens one fd per file and silently drops
// it on certain rename-over-write races), not the primary delivery path, so
// it can run infrequently.
const defaultReconcileInterval = 30 * time.Second

type Watcher struct {
	dir    string
	emit   EmitFunc
	logger *slog.Logger
	ready  chan struct{}
	done   chan struct{}

	// reconcileInterval overrides defaultReconcileInterval; only ever set by
	// tests (unexported, same-package access) to keep the reconciliation pass
	// observable within test timeouts.
	reconcileInterval time.Duration
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

	reconcileInterval := w.reconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}
	// known seeds from the directory's current state before Ready() closes so
	// the first reconcile tick only reports genuine drift, not the initial
	// population of pre-existing files. If that initial scan fails, the first
	// later successful scan establishes the baseline without emitting creates
	// for every already-existing file.
	known, baselineReady := w.snapshot()
	if known == nil {
		known = make(map[string]time.Time)
	}
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

	close(w.ready)

	const debounceInterval = 200 * time.Millisecond

	// Trailing-edge debounce: coalesce bursts of events for the same file
	// and emit a single event after the burst settles. The previous
	// leading-edge implementation silently dropped the last write in a
	// burst, leaving consumers with stale content.
	pending := make(map[string]fsnotify.Op)
	deadlines := make(map[string]time.Time)
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
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}
			// OR ops so a Create+Write burst still surfaces as Create.
			pending[event.Name] |= event.Op
			deadlines[event.Name] = time.Now().Add(debounceInterval)
			timerCh = resetDebounceTimer(timer, deadlines)

		case <-timerCh:
			now := time.Now()
			for name, deadline := range deadlines {
				if deadline.After(now) {
					continue
				}
				op, ok := pending[name]
				if !ok {
					delete(deadlines, name)
					continue
				}
				delete(pending, name)
				delete(deadlines, name)
				w.emitFor(name, op)
			}
			timerCh = resetDebounceTimer(timer, deadlines)
			if w.refreshSnapshot(known) {
				baselineReady = true
			}

		case err, ok := <-fw.Errors:
			if !ok {
				stopTimer(timer)
				return
			}
			w.logger.Error("watcher.error", "err", err)

		case <-reconcile.C:
			baselineReady = w.reconcile(known, pending, baselineReady)
		}
	}
}

// snapshot lists the directory's current .md files and their mtimes. It is
// the reconciliation pass's baseline: anything that changed since the last
// snapshot but produced no fsnotify event is drift the OS-level watch missed.
//
// The bool reports whether the directory scan itself succeeded. A failed
// os.ReadDir (transiently unreadable dir, or a mid-rename during the
// git-checkout recovery that internal/tasksnapshot performs against this same
// directory) returns (nil, false) so callers can skip the pass rather than
// mistake an unreadable directory for an empty one — treating that as "every
// known file was deleted" would fire a board-wide, sticky TaskDeleted storm.
func (w *Watcher) snapshot() (map[string]time.Time, bool) {
	state := make(map[string]time.Time)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		w.logger.Warn("watcher.snapshot.failed", "dir", w.dir, "err", err)
		return nil, false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		state[filepath.Join(w.dir, entry.Name())] = info.ModTime()
	}
	return state, true
}

// refreshSnapshot brings known up to date with whatever the debounce loop
// just emitted, so a legitimately-delivered fsnotify event doesn't also get
// re-reported as drift on the next reconcile tick.
func (w *Watcher) refreshSnapshot(known map[string]time.Time) bool {
	fresh, ok := w.snapshot()
	if !ok {
		// Scan failed; leave known untouched. Clearing it here would make the
		// next reconcile tick re-fire TaskCreated for every file.
		return false
	}
	clear(known)
	maps.Copy(known, fresh)
	return true
}

// reconcile is the self-healing backstop for lost per-file OS watches (see
// defaultReconcileInterval): it diffs the directory's actual state against
// known and synthesizes the fsnotify event that should have fired for any
// file whose mtime moved, appeared, or disappeared without one. This is what
// recovers a file whose kqueue/inotify watch silently died after a
// rename-over-write (e.g. an out-of-band `sed -i` or any tmp+rename atomic
// write) — fsnotify's own directory watch does not re-arm a per-file fd on
// its own for further in-place updates to a file it already lost track of.
func (w *Watcher) reconcile(known map[string]time.Time, pending map[string]fsnotify.Op, baselineReady bool) bool {
	current, ok := w.snapshot()
	if !ok {
		// Scan failed — do not diff against a partial/empty view. Treating an
		// unreadable directory as "empty" would delete-storm every known file.
		return baselineReady
	}
	if !baselineReady {
		clear(known)
		maps.Copy(known, current)
		return true
	}

	for name, mtime := range current {
		// Skip files still inside the debounce window: the pending flush will
		// emit their event and refresh known, so reporting drift here would
		// double-emit the same write.
		if _, inFlight := pending[name]; inFlight {
			continue
		}
		prev, existed := known[name]
		switch {
		case !existed:
			w.logger.Warn("watcher.reconcile.recovered", "op", "created", "file", name)
			w.emitFor(name, fsnotify.Create)
		case !prev.Equal(mtime):
			// mtime-only diff: a write that preserves the mtime (coarse
			// filesystem resolution, or a tool that restores it on save) is
			// invisible to this backstop. Acceptable — the fsnotify path still
			// covers those; reconcile only recovers writes that also moved the
			// mtime but produced no OS event.
			w.logger.Warn("watcher.reconcile.recovered", "op", "updated", "file", name)
			w.emitFor(name, fsnotify.Write)
		}
		known[name] = mtime
	}
	for name := range known {
		if _, inFlight := pending[name]; inFlight {
			continue
		}
		if _, ok := current[name]; !ok {
			w.logger.Warn("watcher.reconcile.recovered", "op", "deleted", "file", name)
			w.emit(events.TaskDeleted, name)
			delete(known, name)
		}
	}
	return true
}

func resetDebounceTimer(timer *time.Timer, deadlines map[string]time.Time) <-chan time.Time {
	if len(deadlines) == 0 {
		stopTimer(timer)
		return nil
	}
	next := nextDeadline(deadlines)
	wait := time.Until(next)
	wait = max(wait, 0)
	stopTimer(timer)
	timer.Reset(wait)
	return timer.C
}

func nextDeadline(deadlines map[string]time.Time) time.Time {
	var next time.Time
	for _, deadline := range deadlines {
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	return next
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
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
