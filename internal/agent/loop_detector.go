package agent

import "sync"

type loopDetector struct {
	mu sync.Mutex

	// Empty signatures preserve streaks. Ack suppresses only the current
	// non-empty signature; a different signature re-arms loop detection.
	lastSig string
	streak  int
	ackSig  string
}

func (d *loopDetector) noteSignature(sig string) int {
	if sig == "" {
		return d.currentStreak()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if sig == d.lastSig {
		d.streak++
	} else {
		d.lastSig = sig
		d.streak = 1
	}
	return d.streak
}

func (d *loopDetector) currentStreak() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.streak
}

func (d *loopDetector) ack() {
	d.mu.Lock()
	d.ackSig = d.lastSig
	d.mu.Unlock()
}

func (d *loopDetector) acknowledged() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ackSig != "" && d.ackSig == d.lastSig
}
