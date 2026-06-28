package agent

import "sync"

type loopDetector struct {
	mu sync.RWMutex

	lastSig string
	streak  int
	ackSig  string
}

func (d *loopDetector) NoteSignature(sig string) int {
	if sig == "" {
		return d.Streak()
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

func (d *loopDetector) Streak() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.streak
}

func (d *loopDetector) Ack() {
	d.mu.Lock()
	d.ackSig = d.lastSig
	d.mu.Unlock()
}

func (d *loopDetector) Acknowledged() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ackSig != "" && d.ackSig == d.lastSig
}
