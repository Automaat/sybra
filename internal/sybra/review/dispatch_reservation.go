package review

// tryReservePRDispatch serializes PR worktree preparation and the handoff to
// either a direct review agent or a durable fix workflow. It is deliberately
// separate from agent.Manager's dispatch claim: workflow run_agent launchers
// acquire that claim themselves, while this reservation must remain held
// until the workflow has durably accepted the prepared checkout.
func (r *Handler) tryReservePRDispatch(taskID string) (release func(), ok bool) {
	r.prDispatchMu.Lock()
	defer r.prDispatchMu.Unlock()
	if _, exists := r.prDispatching[taskID]; exists {
		return nil, false
	}
	if r.prDispatching == nil {
		r.prDispatching = make(map[string]struct{})
	}
	r.prDispatching[taskID] = struct{}{}
	return func() {
		r.prDispatchMu.Lock()
		defer r.prDispatchMu.Unlock()
		delete(r.prDispatching, taskID)
	}, true
}
