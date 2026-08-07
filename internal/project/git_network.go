package project

import (
	"context"
	"time"

	"github.com/Automaat/sybra/internal/errclass"
)

var gitOpRetrySleepContext = sleepWithContext

// IsTransientNetworkError reports whether err looks like a transient
// network/transport failure from a git remote operation (fetch, ls-remote)
// rather than a genuine content conflict, auth failure, or misconfiguration.
func IsTransientNetworkError(err error) bool {
	return errclass.ClassifyErr(err, errclass.GitTransportEscalationBiased) == errclass.Transient
}

// withNetworkRetry runs fn, retrying on gitOpRetryBackoffs when the failure
// looks like a transient network error. Non-network errors return
// immediately without retrying. Shares its backoff schedule with
// withLockRetry — both are bounded, short retries for conditions that
// typically clear within seconds.
func withNetworkRetry(ctx context.Context, fn func() error) error {
	err := fn()
	for attempt := 0; attempt < len(gitOpRetryBackoffs) && IsTransientNetworkError(err); attempt++ {
		if sleepErr := gitOpRetrySleepContext(ctx, gitOpRetryBackoffs[attempt]); sleepErr != nil {
			return sleepErr
		}
		err = fn()
	}
	return err
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
