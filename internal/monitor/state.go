package monitor

import (
	"sync"
	"time"
)

// runState tracks per-fingerprint timestamps the Service uses to throttle
// repeat work across ticks. It is the only mutable state on the Service that
// outlives a single tick. Cross-process dedup is handled by the IssueSink's gh
// query — runState exists only to avoid pinging gh more than once per cooldown.
type runState struct {
	mu                sync.Mutex
	lastIssueAt       map[string]time.Time
	lastDispatchAt    map[string]time.Time
	lastReport        Report
	lastReportAt      time.Time
	lastReportInitial bool
}

func newRunState() *runState {
	return &runState{
		lastIssueAt:    make(map[string]time.Time),
		lastDispatchAt: make(map[string]time.Time),
	}
}

// canIssue reports whether the fingerprint has cleared the cooldown window.
// Records the current time on a positive answer so callers don't have to.
func (s *runState) canIssue(fp string, now time.Time, cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastIssueAt[fp]; ok && now.Sub(last) < cooldown {
		return false
	}
	s.lastIssueAt[fp] = now
	return true
}

// canDispatch is the cooldown gate for LLM dispatch. Rationale matches
// canIssue: a flapping anomaly should not spawn a Claude session every tick.
func (s *runState) canDispatch(fp string, now time.Time, cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastDispatchAt[fp]; ok && now.Sub(last) < cooldown {
		return false
	}
	s.lastDispatchAt[fp] = now
	return true
}

// recordReport stores the most recent finished report so Wails callers can
// fetch it without waiting for the next tick.
func (s *runState) recordReport(r Report, at time.Time) {
	s.mu.Lock()
	s.lastReport = r
	s.lastReportAt = at
	s.lastReportInitial = true
	s.mu.Unlock()
}

func (s *runState) snapshot() (Report, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReport, s.lastReportAt, s.lastReportInitial
}
