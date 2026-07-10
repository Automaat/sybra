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

	// verifyCommitsRetryBackoff is the delay before a single retry of
	// `git log` in verify_commits — verify_commits runs immediately after
	// the agent process exits, so a leftover .git/index.lock can transiently
	// fail the first call. Indirected for tests.
	verifyCommitsRetryBackoff = 500 * time.Millisecond
	verifyCommitsRetrySleep   = time.Sleep

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
