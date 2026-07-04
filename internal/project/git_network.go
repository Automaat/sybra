package project

import "strings"

// transientNetworkMarkers are substrings of git/ssh/curl transport failures
// that indicate a connectivity blip (DNS, refused/reset connection, timeout)
// rather than a genuine content conflict or an auth/config problem. Matching
// is deliberately narrow — a false positive here would retry (and briefly
// delay reporting) a permanent failure, but a false negative would wrongly
// classify a transient outage as a content conflict and escalate a task to
// human-required on a perfectly clean branch.
var transientNetworkMarkers = []string{
	"connection refused",
	"connection reset",
	"connection timed out",
	"could not resolve host",
	"couldn't resolve host",
	"network is unreachable",
	"operation timed out",
	"temporary failure in name resolution",
	"no route to host",
	"ssh: connect to host",
	"recv failure",
	"tls handshake timeout",
	"empty reply from server",
}

// IsTransientNetworkError reports whether err looks like a transient
// network/transport failure from a git remote operation (fetch, ls-remote)
// rather than a genuine content conflict, auth failure, or misconfiguration.
func IsTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientNetworkMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// withNetworkRetry runs fn, retrying on gitOpRetryBackoffs when the failure
// looks like a transient network error. Non-network errors return
// immediately without retrying. Shares its backoff schedule with
// withLockRetry — both are bounded, short retries for conditions that
// typically clear within seconds.
func withNetworkRetry(fn func() error) error {
	err := fn()
	for attempt := 0; attempt < len(gitOpRetryBackoffs) && IsTransientNetworkError(err); attempt++ {
		gitOpRetrySleep(gitOpRetryBackoffs[attempt])
		err = fn()
	}
	return err
}
