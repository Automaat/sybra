package agent

// FilterOpts holds the pre-computed set of already-tracked agent identifiers.
// All maps are keyed by the relevant identifier; zero value is safe (no sessions filtered).
type FilterOpts struct {
	TrackedPIDs       map[int]bool
	TrackedPaths      map[string]bool
	TrackedSessionIDs map[string]bool
}

// filterSessions is a pure function: no I/O, no side effects.
// It returns sessions that are not already tracked and are not definitively terminated.
func filterSessions(sessions []RawSession, opts FilterOpts) []RawSession {
	var result []RawSession
	for i := range sessions {
		s := &sessions[i]
		if s.Terminated {
			continue
		}
		if s.PID != 0 && opts.TrackedPIDs[s.PID] {
			continue
		}
		if s.FilePath != "" && opts.TrackedPaths[s.FilePath] {
			continue
		}
		if s.SessionID != "" && opts.TrackedSessionIDs[s.SessionID] {
			continue
		}
		result = append(result, *s)
	}
	return result
}
