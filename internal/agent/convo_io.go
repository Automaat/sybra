package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// convoIO owns the conversational agent transport plus the existing
// prompt/restart bookkeeping mechanically extracted from Agent.
type convoIO struct {
	stdinPipe io.WriteCloser
	stdinMu   sync.Mutex
	hasPipe   atomic.Bool

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
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()

	if c.stdinPipe == nil {
		return fmt.Errorf("stdin pipe closed")
	}
	// A write larger than the pipe buffer can block under stdinMu until the
	// child drains stdin; keep stdinPipe ownership here so callers cannot race.
	if _, err := c.stdinPipe.Write(data); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
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
