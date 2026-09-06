package agent

// GetRemoteCompletionReceipt returns the terminal identity observed through
// the execution backend. It is not durable until the completion handler stores
// it with the canonical run result and cost.
func (a *Agent) GetRemoteCompletionReceipt() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.remoteCompletionReceipt
}
