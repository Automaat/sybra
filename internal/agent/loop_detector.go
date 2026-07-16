package agent

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

const (
	loopWindowSize          = 12
	maxLoopCycleFamilies    = 2
	maxLoopEvidenceExamples = 3
)

type ToolLoopEvidence struct {
	Signature      string
	Label          string
	Count          int
	Window         int
	UniqueFamilies int
	Examples       []string
}

func (e ToolLoopEvidence) Summary() string {
	if e.Count == 0 || e.Window == 0 {
		return ""
	}
	if e.UniqueFamilies > 1 {
		return fmt.Sprintf("%d of last %d low-progress actions cycled across %d semantic families: %s",
			e.Count, e.Window, e.UniqueFamilies, strings.Join(e.Examples, " | "))
	}
	if len(e.Examples) > 1 {
		return fmt.Sprintf("%d of last %d low-progress actions repeated %s (%s)",
			e.Count, e.Window, e.Label, strings.Join(e.Examples, " | "))
	}
	return fmt.Sprintf("%d of last %d low-progress actions repeated %s", e.Count, e.Window, e.Label)
}

type loopDetector struct {
	mu sync.Mutex

	// Empty signatures preserve the current low-progress window. Ack suppresses
	// only the current semantic-loop signature; a progress reset or a different
	// window re-arms loop detection.
	recent    []loopAction
	lastSig   string
	lastLabel string
	streak    int
	ackSig    string
}

type loopAction struct {
	signature string
	label     string
}

type familyStats struct {
	count    int
	lastSeen int
	label    string
}

func (d *loopDetector) noteAction(sig, label string) int {
	if sig == "" {
		return d.currentEvidence().Count
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.recent = append(d.recent, loopAction{signature: sig, label: label})
	if len(d.recent) > loopWindowSize {
		d.recent = slices.Clone(d.recent[len(d.recent)-loopWindowSize:])
	}
	if sig == d.lastSig {
		if d.streak < len(d.recent) {
			d.streak++
		}
	} else {
		d.lastSig = sig
		d.lastLabel = label
		d.streak = 1
	}
	return d.currentEvidenceLocked().Count
}

func (d *loopDetector) noteProgress() {
	d.mu.Lock()
	d.recent = nil
	d.lastSig = ""
	d.lastLabel = ""
	d.streak = 0
	d.ackSig = ""
	d.mu.Unlock()
}

func (d *loopDetector) currentStreak() int {
	return d.currentEvidence().Count
}

func (d *loopDetector) ack() {
	d.mu.Lock()
	d.ackSig = d.currentEvidenceLocked().Signature
	d.mu.Unlock()
}

func (d *loopDetector) acknowledged() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	evidence := d.currentEvidenceLocked()
	return d.ackSig != "" && d.ackSig == evidence.Signature
}

func (d *loopDetector) currentEvidence() ToolLoopEvidence {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.currentEvidenceLocked()
}

func (d *loopDetector) currentEvidenceLocked() ToolLoopEvidence {
	if len(d.recent) == 0 {
		return ToolLoopEvidence{}
	}

	stats := make(map[string]*familyStats, len(d.recent))
	labels := make([]string, 0, len(d.recent))
	for i := range d.recent {
		action := d.recent[i]
		entry := stats[action.signature]
		if entry == nil {
			entry = &familyStats{}
			stats[action.signature] = entry
		}
		entry.count++
		entry.lastSeen = i
		if action.label != "" {
			entry.label = action.label
		}
		labels = append(labels, action.label)
	}

	if len(stats) == maxLoopCycleFamilies && len(d.recent) >= 4 && cycleLooksBalanced(stats) {
		examples := compactUnique(labels)
		if len(examples) > maxLoopEvidenceExamples {
			examples = examples[:maxLoopEvidenceExamples]
		}
		cycleLabels := compactUnique(labels)
		slices.Sort(cycleLabels)
		return ToolLoopEvidence{
			Signature:      "cycle:" + hashParts(sortedMapKeys(stats)),
			Label:          strings.Join(cycleLabels, " + "),
			Count:          len(d.recent),
			Window:         len(d.recent),
			UniqueFamilies: len(stats),
			Examples:       examples,
		}
	}

	examples := make([]string, 0, maxLoopEvidenceExamples)
	seen := map[string]struct{}{}
	for i := range d.recent {
		action := d.recent[i]
		if action.signature != d.lastSig || action.label == "" {
			continue
		}
		if _, ok := seen[action.label]; ok {
			continue
		}
		seen[action.label] = struct{}{}
		examples = append(examples, action.label)
		if len(examples) == maxLoopEvidenceExamples {
			break
		}
	}
	label := d.lastLabel
	if label == "" && len(examples) > 0 {
		label = examples[0]
	}
	return ToolLoopEvidence{
		Signature:      d.lastSig,
		Label:          label,
		Count:          d.streak,
		Window:         len(d.recent),
		UniqueFamilies: len(stats),
		Examples:       examples,
	}
}

func sortedMapKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func cycleLooksBalanced(stats map[string]*familyStats) bool {
	counts := make([]int, 0, len(stats))
	for _, entry := range stats {
		counts = append(counts, entry.count)
	}
	slices.Sort(counts)
	return counts[len(counts)-1]-counts[0] <= 1
}
