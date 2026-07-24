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
	lostAgent         map[string]*lostAgentTrack
}

func newRunState() *runState {
	return &runState{
		lastIssueAt:    make(map[string]time.Time),
		lastDispatchAt: make(map[string]time.Time),
		lostAgent:      make(map[string]*lostAgentTrack),
	}
}

// lostAgentTrack records the consecutive-tick history of one lost_agent
// fingerprint (kind+taskID) so the service can gate issue filing on repeated
// detection despite remediation (not first detection) and auto-close once
// the condition clears. Keyed in runState.lostAgent by the base fingerprint,
// which stays stable even when the issue actually filed used a
// cause-qualified fingerprint (filedFP) — e.g. a remediation error.
type lostAgentTrack struct {
	taskID      string
	hitStreak   int
	clearStreak int
	filed       bool
	filedFP     string
}

// lostAgentHit records a detection of fp (the base kind+taskID fingerprint)
// on the current tick, resets its clear streak, and returns the new
// consecutive-hit count.
func (s *runState) lostAgentHit(fp, taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.lostAgent[fp]
	if t == nil {
		t = &lostAgentTrack{taskID: taskID}
		s.lostAgent[fp] = t
	}
	t.hitStreak++
	t.clearStreak = 0
	return t.hitStreak
}

// lostAgentFiled records that an issue (or local task) now exists for fp,
// filed under filedFP (which may carry a ":cause" suffix fp itself lacks).
func (s *runState) lostAgentFiled(fp, filedFP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.lostAgent[fp]; t != nil {
		t.filed = true
		t.filedFP = filedFP
	}
}

// lostAgentFiledFP returns the fingerprint an issue/task was actually filed
// under for fp, if one is currently open. Used so every subsequent hit on an
// already-filed fingerprint keeps commenting on that same issue instead of
// re-evaluating the cause/streak gate — which, once a transient remediation
// error clears, could otherwise open a second, differently-fingerprinted
// issue for a condition that never actually cleared.
func (s *runState) lostAgentFiledFP(fp string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.lostAgent[fp]
	if t == nil || !t.filed {
		return "", false
	}
	return t.filedFP, true
}

// lostAgentClear is one fingerprint whose tracked issue/task just qualified
// for auto-close: its condition has stayed clear for closeAfterClears
// consecutive ticks.
type lostAgentClear struct {
	taskID  string
	filedFP string
}

// lostAgentSweepClears advances the clear streak for every previously-hit
// lost_agent fingerprint absent from seenThisTick, and returns the ones whose
// filed issue/task just crossed closeAfterClears — those are forgotten
// afterward so a future recurrence starts a clean streak. Fingerprints that
// were never actually filed (fully self-healed before the occurrence
// threshold) are dropped on their first clear tick so tracking doesn't grow
// without bound for every transient blip.
func (s *runState) lostAgentSweepClears(seenThisTick map[string]bool, closeAfterClears int) []lostAgentClear {
	s.mu.Lock()
	defer s.mu.Unlock()
	var closed []lostAgentClear
	for fp, t := range s.lostAgent {
		if seenThisTick[fp] {
			continue
		}
		t.clearStreak++
		t.hitStreak = 0
		if !t.filed {
			delete(s.lostAgent, fp)
			continue
		}
		if closeAfterClears > 0 && t.clearStreak >= closeAfterClears {
			closed = append(closed, lostAgentClear{taskID: t.taskID, filedFP: t.filedFP})
			delete(s.lostAgent, fp)
		}
	}
	return closed
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
