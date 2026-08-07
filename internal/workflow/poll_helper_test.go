package workflow

import "time"

// pollUntil reports whether cond becomes true within timeout, checking
// immediately and then on every interval tick. It centralizes the
// deadline-poll idiom the workflow tests use to wait on state mutated by a
// background goroutine or an external subprocess (a real OS process's
// wall-clock progress can't be faked by testing/synctest), so retry cadence
// lives in one place instead of a scattered time.Sleep-in-a-loop per test.
func pollUntil(timeout, interval time.Duration, cond func() bool) bool {
	if cond() {
		return true
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		if cond() {
			return true
		}
	}
	return false
}
