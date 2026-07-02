package project

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bareRepoLocks serializes git mutations against a single bare clone within
// this process. When N task worktrees are prepared concurrently off the same
// bare repo, their fetch/worktree-add/prune calls otherwise race on the
// repo's shared lock files (e.g. .git/index.lock, ref locks). Keyed by the
// cleaned clone path so callers passing equivalent-but-differently-formatted
// paths still serialize against each other.
var bareRepoLocks sync.Map // map[string]*sync.Mutex

// withBareRepoLock runs fn while holding the mutex for barePath, blocking
// concurrent git mutations against the same bare clone from other goroutines
// in this process. It does not protect against another OS process (e.g. a
// second Sybra instance) touching the same clone — see gitOpRetryBackoffs for
// that cross-process safety net.
func withBareRepoLock(barePath string, fn func() error) error {
	key := filepath.Clean(barePath)
	v, _ := bareRepoLocks.LoadOrStore(key, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		// Unreachable: bareRepoLocks only ever stores *sync.Mutex values.
		mu = &sync.Mutex{}
	}
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

// gitOpRetryBackoffs bounds retries for bare-repo git mutations that hit a
// transient lock file left by a concurrent process touching the same bare
// clone (e.g. .git/index.lock, a ref lock, or a worktree admin lock). The
// in-process mutex above serializes same-process callers; this retry is the
// backstop for cross-process races — e.g. a desktop app and a server instance
// both preparing worktrees against one shared bare clone. Indirected for
// tests.
var (
	gitOpRetryBackoffs = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
	gitOpRetrySleep    = time.Sleep
)

// isLockContention reports whether err looks like a transient git lock-file
// failure ("Unable to create '.../index.lock': File exists.", or similar for
// ref/worktree admin locks) rather than a genuine, non-retriable failure such
// as a permission error on the lock file itself.
func isLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, ".lock") && strings.Contains(msg, "File exists")
}

// withLockRetry runs fn, retrying on gitOpRetryBackoffs when the failure looks
// like lock contention. Non-lock errors return immediately without retrying.
func withLockRetry(fn func() error) error {
	err := fn()
	for attempt := 0; attempt < len(gitOpRetryBackoffs) && isLockContention(err); attempt++ {
		gitOpRetrySleep(gitOpRetryBackoffs[attempt])
		err = fn()
	}
	return err
}
