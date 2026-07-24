package poll

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestPollerPollOnceRecoversPanic(t *testing.T) {
	t.Parallel()

	p := New(&panicFetcher{}, 0, slog.New(slog.DiscardHandler))
	if got := p.pollOnce(context.Background()); got != time.Minute {
		t.Fatalf("pollOnce() = %s, want %s after panic recovery", got, time.Minute)
	}
}

type panicFetcher struct{}

func (*panicFetcher) Name() string { return "panic" }

func (*panicFetcher) Poll(context.Context) time.Duration {
	panic("boom")
}
