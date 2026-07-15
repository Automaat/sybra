package selfmonitor

import (
	"sync"
	"time"
)

// runState tracks the in-memory snapshot the Service exposes to Wails
// callers between ticks. Phase C will grow this struct with per-fingerprint
// cooldown maps once the IssueSink is wired in.
type runState struct {
	mu                sync.Mutex
	lastReport        Report
	lastReportAt      time.Time
	lastReportInitial bool
}

func newRunState() *runState {
	return &runState{}
}

// recordReport stores the most recent finished report so callers can fetch
// it without waiting for the next tick.
func (s *runState) recordReport(r Report, at time.Time) {
	s.mu.Lock()
	s.lastReport = r
	s.lastReportAt = at
	s.lastReportInitial = true
	s.mu.Unlock()
}

// snapshot returns the latest stored report, its timestamp, and whether a
// tick has completed since startup.
func (s *runState) snapshot() (Report, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReport, s.lastReportAt, s.lastReportInitial
}
