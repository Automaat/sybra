package agent

import (
	"io"
	"sync"
)

type convoIO struct {
	stdinPipe io.WriteCloser
	stdinMu   sync.Mutex

	approvalCh chan ApprovalResponse

	// stdinPath is the FIFO backing a detached conversational agent's stdin,
	// reopened on reattach so follow-up messages survive a restart. Empty for
	// pipe-backed (non-survival) agents. Guarded by Agent.mu.
	stdinPath string

	// pendingPrompts queues follow-up user messages that arrive while a turn is
	// mid-flight. Drained after each "result" event so the next turn fires
	// without waiting on the user. Guarded by Agent.mu.
	pendingPrompts []string

	// promptCh delivers follow-up prompts to Codex conversational agents. Each
	// turn spawns a new codex exec process; promptCh signals the next prompt
	// without a stdin pipe. Guarded by Agent.mu.
	promptCh chan string
}

func (c *convoIO) setStdinPipe(pipe io.WriteCloser) {
	c.stdinMu.Lock()
	c.stdinPipe = pipe
	c.stdinMu.Unlock()
}

func (c *convoIO) replaceStdinPipe(pipe io.WriteCloser) {
	c.stdinMu.Lock()
	if c.stdinPipe != nil {
		_ = c.stdinPipe.Close()
	}
	c.stdinPipe = pipe
	c.stdinMu.Unlock()
}

func (c *convoIO) closeStdinPipe() {
	c.stdinMu.Lock()
	if c.stdinPipe != nil {
		_ = c.stdinPipe.Close()
		c.stdinPipe = nil
	}
	c.stdinMu.Unlock()
}

func (c *convoIO) hasStdinPipe() bool {
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()
	return c.stdinPipe != nil
}
