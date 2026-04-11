package poll

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Fetcher runs one poll cycle. The returned duration sets the next interval.
// Returning 0 or negative falls back to one minute.
type Fetcher interface {
	Name() string
	Poll(ctx context.Context) time.Duration
}

// Poller drives a single Fetcher on a timer.
type Poller struct {
	fetcher     Fetcher
	initialWait time.Duration
	logger      *slog.Logger
}

// New returns a Poller for f with the given initial delay before the first tick.
func New(f Fetcher, initialWait time.Duration, logger *slog.Logger) *Poller {
	return &Poller{fetcher: f, initialWait: initialWait, logger: logger}
}

// Run blocks until ctx is cancelled, calling f.Poll on each tick.
func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(p.initialWait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := p.fetcher.Poll(ctx)
			if next <= 0 {
				next = time.Minute
			}
			p.logger.Debug("poll.next", "name", p.fetcher.Name(), "interval", next)
			timer.Reset(next)
		}
	}
}

// Hub owns a set of Fetchers and starts them together.
type Hub struct {
	entries []hubEntry
}

type hubEntry struct {
	fetcher     Fetcher
	initialWait time.Duration
}

// NewHub returns an empty Hub.
func NewHub() *Hub { return &Hub{} }

// Register adds a Fetcher to the Hub.
func (h *Hub) Register(f Fetcher, initialWait time.Duration) {
	h.entries = append(h.entries, hubEntry{f, initialWait})
}

// Start launches each registered Fetcher as a goroutine tracked by wg.
func (h *Hub) Start(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger) {
	for _, e := range h.entries {
		p := New(e.fetcher, e.initialWait, logger)
		wg.Go(func() { p.Run(ctx) })
	}
}
