package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestShouldSkipOptional_PriorityTiers(t *testing.T) {
	far := time.Now().Add(time.Hour)

	tests := []struct {
		name          string
		remaining     int
		limit         int
		wantMerge     bool
		wantDiscovery bool
	}{
		{name: "well above reserve", remaining: 1001, limit: 5000, wantMerge: false, wantDiscovery: false},
		{name: "at reserve boundary", remaining: 1000, limit: 5000, wantMerge: false, wantDiscovery: true},
		{name: "mid reserved band", remaining: 800, limit: 5000, wantMerge: false, wantDiscovery: true},
		{name: "just above floor", remaining: 151, limit: 5000, wantMerge: false, wantDiscovery: true},
		{name: "at floor boundary", remaining: 150, limit: 5000, wantMerge: true, wantDiscovery: true},
		{name: "unknown limit at floor", remaining: 140, limit: 0, wantMerge: true, wantDiscovery: true},
		{name: "unknown limit above floor", remaining: 200, limit: 0, wantMerge: false, wantDiscovery: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGHRequestGate()
			g.resources["graphql"] = ghRateWindow{
				remaining: tt.remaining,
				limit:     tt.limit,
				resetAt:   far,
			}
			if got := g.shouldSkipOptional("graphql", priorityMergePath); got != tt.wantMerge {
				t.Errorf("merge-path shouldSkipOptional = %v, want %v", got, tt.wantMerge)
			}
			if got := g.shouldSkipOptional("graphql", priorityDiscovery); got != tt.wantDiscovery {
				t.Errorf("discovery shouldSkipOptional = %v, want %v", got, tt.wantDiscovery)
			}
		})
	}

	t.Run("notBefore hard wall skips every tier", func(t *testing.T) {
		g := newGHRequestGate()
		g.resources["graphql"] = ghRateWindow{remaining: 4000, limit: 5000, resetAt: far}
		g.notBefore = time.Now().Add(time.Hour)
		if !g.shouldSkipOptional("graphql", priorityMergePath) {
			t.Error("want merge-path skipped under notBefore wall")
		}
		if !g.shouldSkipOptional("graphql", priorityDiscovery) {
			t.Error("want discovery skipped under notBefore wall")
		}
	})
}

// TestFetchReviews_DiscoverySkipsInReservedBand asserts that a discovery poll
// sheds its GraphQL request (rather than degrading to REST, which the review
// summary has no equivalent for) once the budget is inside the reserved band
// that merge-path polls still get to spend.
func TestFetchReviews_DiscoverySkipsInReservedBand(t *testing.T) {
	origGate := ghGate
	origExecer := defaultExecer
	origCache := reviewSummaryCache
	t.Cleanup(func() {
		ghGate = origGate
		defaultExecer = origExecer
		reviewSummaryCache = origCache
	})

	ghGate = newGHRequestGate()
	ghGate.resources["graphql"] = ghRateWindow{remaining: 800, limit: 5000, resetAt: time.Now().Add(time.Hour)}
	reviewSummaryCache = newTTLCache[ReviewSummary]()

	e := &fakeExecer{output: []byte(`{"errors":[{"message":"should not be called"}]}`)}
	defaultExecer = e

	_, err := fetchReviewsWith(defaultExecer)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if e.calls != 0 {
		t.Errorf("calls = %d, want 0 (no GraphQL search should fire in the reserved band)", e.calls)
	}
}

// TestFetchPRForMonitor_MergePathContinuesInReservedBand asserts that a
// merge-path poll keeps using GraphQL (not the REST fallback) throughout the
// reserved band, since merge-path only degrades at the ghLowBudgetThreshold
// floor.
func TestFetchPRForMonitor_MergePathContinuesInReservedBand(t *testing.T) {
	origGate := ghGate
	origExecer := defaultExecer
	t.Cleanup(func() {
		ghGate = origGate
		defaultExecer = origExecer
	})

	ghGate = newGHRequestGate()
	ghGate.resources["graphql"] = ghRateWindow{remaining: 800, limit: 5000, resetAt: time.Now().Add(time.Hour)}

	body := `{
		"data": {
			"viewer": {"login": "me"},
			"repository": {
				"pullRequest": {
					"number": 7,
					"title": "in review",
					"url": "https://github.com/o/r/pull/7",
					"state": "OPEN",
					"author": {"login": "peer", "type": "User"},
					"repository": {"name": "r", "nameWithOwner": "o/r"},
					"labels": {"nodes": []},
					"commits": {"nodes": []},
					"reviewThreads": {"nodes": []},
					"latestReviews": {"nodes": []}
				}
			}
		}
	}`
	rec := &recordingExecer{output: []byte(body)}
	defaultExecer = rec

	pr, open, err := fetchPRForMonitorWith(defaultExecer, "o/r", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !open {
		t.Error("open = false, want true")
	}
	if pr.Number != 7 {
		t.Errorf("Number = %d, want 7", pr.Number)
	}
	if rec.calls != 1 {
		t.Fatalf("calls = %d, want 1", rec.calls)
	}
	if !containsArg(rec.lastArgs, "graphql") {
		t.Errorf("lastArgs = %v, want a graphql call (merge-path must not fall back to REST in the reserved band)", rec.lastArgs)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// TestGHRequestGateExecute_SuppressesCallsWhileAuthCircuitOpen asserts the
// gate's chokepoint (shared by every gh invocation in this package) skips
// shelling out entirely while the centralized auth circuit is open, instead
// of repeating a doomed request every ghRequestSpacing tick.
func TestGHRequestGateExecute_SuppressesCallsWhileAuthCircuitOpen(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	authHealth.setState(AuthUnavailable, "test")

	g := newGHRequestGate()
	calls := 0
	out, err := g.execute(context.Background(), func() ([]byte, error) {
		calls++
		return []byte("should not run"), nil
	})
	if calls != 0 {
		t.Fatalf("run() invoked %d times, want 0 while circuit is open", calls)
	}
	if out != nil {
		t.Fatalf("out = %q, want nil", out)
	}
	if !IsAuthError(err) {
		t.Fatalf("err = %v, want an auth-classified circuit-open error", err)
	}
	if got := AuthHealthSnapshot().SuppressedCalls; got != 1 {
		t.Fatalf("SuppressedCalls = %d, want 1", got)
	}
}

func TestGHRequestGateExecute_RateWallDoesNotBlockMutex(t *testing.T) {
	g := newGHRequestGate()
	g.notBefore = time.Now().Add(time.Hour)
	started := time.Now()
	_, err := g.execute(context.Background(), func() ([]byte, error) { t.Fatal("run must be suppressed"); return nil, nil })
	if !strings.Contains(err.Error(), rateLimitWallMarker) {
		t.Fatalf("err = %v, want rate-limit wall", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("rate wall blocked caller")
	}
	done := make(chan struct{})
	go func() { _ = g.shouldSkipOptional("core", priorityMergePath); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rate wall held gate mutex")
	}
}

func TestGHRequestGateExecute_WaitHonorsContext(t *testing.T) {
	g := newGHRequestGate()
	g.lastRun = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.execute(ctx, func() ([]byte, error) { t.Fatal("run must not start"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

func TestGHRequestGateExecute_CanceledContextNeverRuns(t *testing.T) {
	g := newGHRequestGate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.execute(ctx, func() ([]byte, error) { t.Fatal("run must not start"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

func TestGHRequestGateExecute_LockHonorsContext(t *testing.T) {
	g := newGHRequestGate()
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := g.execute(ctx, func() ([]byte, error) { t.Fatal("run must not start"); return nil, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// TestGHRequestGateExecute_ObservesCallResult asserts a real (non-suppressed)
// call result flows into the shared auth-health tracker, so a caller that
// never explicitly wires ObserveCallResult still gets auth-failure detection
// for free by going through the gate.
func TestGHRequestGateExecute_ObservesCallResult(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	t.Cleanup(DisableAppAuth)
	resetAuthHealthForTest()
	DisableAppAuth()

	g := newGHRequestGate()
	_, _ = g.execute(context.Background(), func() ([]byte, error) {
		return []byte("gh: HTTP 401: Bad credentials"), fmt.Errorf("exit status 1")
	})

	if got := AuthHealthSnapshot().State; got != AuthUnavailable {
		t.Fatalf("State after a 401 through execute() = %q, want unavailable", got)
	}
}
