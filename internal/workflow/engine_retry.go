package workflow

import "time"

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
)
