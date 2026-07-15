package workflow

import (
	"context"
	"time"
)

// prVerifyBackoffs controls the retry schedule after editing a PR
// body to add a closing reference. GitHub populates
// closingIssuesReferences asynchronously, so a verify fetch right
// after gh pr edit commonly reads stale data; back off and retry.
// Indirected for tests — test init swaps in zeros to skip real waits.
var (
	prVerifyBackoffs = []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second}
	prVerifySleep    = time.Sleep

	// verifyCommitsRetryBackoffs schedule bounded retries of transient
	// ref/object visibility failures in verify_commits. The gate runs
	// immediately after the agent process exits, so sandbox sync, ref writes,
	// or leftover git locks can briefly make HEAD or its object unreadable.
	// Indirected for tests.
	verifyCommitsRetryBackoffs = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	verifyCommitsRetrySleep    = time.Sleep

	// classifyTaskRetryBackoffs schedules retries after a transient classify
	// failure (rate limit, brief provider outage). classify_task replaced a
	// run_agent step that carried max_retries: 3; this restores an equivalent
	// budget so a transient blip no longer permanently parks the task on
	// human-required. Indirected for tests — test init swaps in zeros.
	classifyTaskRetryBackoffs = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

	// classifyTaskWait blocks for d or until ctx is done, whichever comes
	// first, so a retry backoff never outlives an engine shutdown. Indirected
	// for tests.
	classifyTaskWait = func(ctx context.Context, d time.Duration) {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
)
