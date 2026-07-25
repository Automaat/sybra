package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// stdinWriteTimeout bounds how long a single writeStdin call may block on a
// wedged child process. Kept well under the watchdog's smallest per-role
// stall ceiling (10m, see internal/watchdog.stallLimit) so a stuck write
// self-recovers long before the watchdog would notice the resulting
// inactivity and try to stop the agent through the same pipe.
const stdinWriteTimeout = 30 * time.Second

// convoIO owns the conversational agent transport plus the existing
// prompt/restart bookkeeping mechanically extracted from Agent.
type convoIO struct {
	// stdinPipe/hasPipe are the current stdin handle. stdinMu guards only
	// handle bookkeeping (install/replace/close, plus the pointer snapshot
	// writeStdin takes before writing) — it is never held across the
	// blocking OS write itself, so a wedged write can never block
	// installStdinPipe/replaceStdinPipe/closeStdinPipe. Those callers
	// (StopAgent, markAgentDone, the watchdog's stall-recovery path) must
	// always be able to make progress even while a write is stuck.
	stdinPipe io.WriteCloser
	stdinMu   sync.Mutex
	hasPipe   atomic.Bool

	// writeMu serializes the stdin writes themselves so concurrent callers
	// cannot interleave partial writes into the child's stream-json input.
	// Deliberately a separate lock from stdinMu — see the comment above.
	writeMu sync.Mutex

	// stdinPath is the FIFO backing a detached conversational agent's stdin,
	// reopened on reattach so follow-up messages survive a restart. Empty for
	// pipe-backed (non-survival) agents. Guarded by Agent.mu.
	stdinPath string

	// pendingPrompts queues follow-up user messages that arrive while a turn is
	// mid-flight. Drained after each "result" event so the next turn fires
	// without waiting on the user. Guarded by Agent.mu.
	pendingPrompts []string
}

func (c *convoIO) installStdinPipe(pipe io.WriteCloser) error {
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()

	if c.stdinPipe != nil {
		return fmt.Errorf("stdin pipe already installed")
	}
	c.stdinPipe = pipe
	c.hasPipe.Store(pipe != nil)
	return nil
}

func (c *convoIO) replaceStdinPipe(pipe io.WriteCloser) {
	c.stdinMu.Lock()
	if c.stdinPipe != nil {
		_ = c.stdinPipe.Close()
	}
	c.stdinPipe = pipe
	c.hasPipe.Store(pipe != nil)
	c.stdinMu.Unlock()
}

func (c *convoIO) writeStdin(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.stdinMu.Lock()
	pipe := c.stdinPipe
	c.stdinMu.Unlock()

	if pipe == nil {
		return fmt.Errorf("stdin pipe closed")
	}

	// A write larger than the pipe buffer can block until the child drains
	// stdin. Run it off a timer instead of inline so a wedged child cannot
	// hold this call forever; on timeout, force-close the pipe so the
	// blocked OS write unblocks (Close interrupts a pending Read/Write on
	// the same *os.File) and any following write/close call proceeds
	// against a clean pipe instead of piling up behind this one.
	done := make(chan error, 1)
	go func() {
		_, err := pipe.Write(data)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("write stdin: %w", err)
		}
		return nil
	case <-time.After(stdinWriteTimeout):
		c.forceClosePipe(pipe)
		return fmt.Errorf("write stdin: timed out after %s, pipe closed", stdinWriteTimeout)
	}
}

// forceClosePipe closes pipe and clears it from convoIO's bookkeeping, but
// only if it is still the active stdin handle. A concurrent
// replaceStdinPipe/closeStdinPipe call may already have moved convoIO on to
// a different (or no) pipe by the time a wedged write's timeout fires, in
// which case this is a no-op so a stale timeout cannot clobber a newer pipe.
func (c *convoIO) forceClosePipe(pipe io.WriteCloser) {
	c.stdinMu.Lock()
	if c.stdinPipe == pipe {
		c.stdinPipe = nil
		c.hasPipe.Store(false)
	}
	c.stdinMu.Unlock()
	_ = pipe.Close()
}

func (c *convoIO) closeStdinPipe() {
	c.stdinMu.Lock()
	if c.stdinPipe != nil {
		_ = c.stdinPipe.Close()
		c.stdinPipe = nil
		c.hasPipe.Store(false)
	}
	c.stdinMu.Unlock()
}

func (c *convoIO) hasStdinPipe() bool {
	return c.hasPipe.Load()
}

// encodeUserMessage renders a user message as a newline-terminated
// stream-json line for claude's stdin.
func encodeUserMessage(text string) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return append(data, '\n'), nil
}

// writeUserMessage writes a user message to the agent's stdin in stream-json format.
func (m *Manager) writeUserMessage(a *Agent, text string) error {
	data, err := encodeUserMessage(text)
	if err != nil {
		return err
	}

	return a.convo.writeStdin(data)
}
