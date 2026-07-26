package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/workflow"
)

// StatusEffect is one observer-owned task mutation recorded durably in
// Task.EffectLog before it is considered applied.
type StatusEffect struct {
	Source string
	Update Update
}

// ApplyStatusEffect applies a poller/watchdog/recovery-owned status mutation
// exactly once per consumed task generation and logical effect signature.
// Replays of the same Source+Update after completion become a no-op until
// another task mutation advances the generation.
func (m *Manager) ApplyStatusEffect(id string, eff StatusEffect) (Task, error) {
	source := strings.TrimSpace(eff.Source)
	if source == "" {
		return Task{}, fmt.Errorf("apply status effect: source is required")
	}
	if eff.Update.Status == nil {
		return Task{}, fmt.Errorf("apply status effect: update.status is required")
	}

	mu := m.lockFor(id)
	mu.Lock()

	cur, err := m.store.Get(id)
	if err != nil {
		mu.Unlock()
		return Task{}, err
	}

	stepID := statusEffectStepID(source, eff.Update)
	if statusEffectApplied(cur.EffectLog, cur.Generation-1, stepID) {
		mu.Unlock()
		return cur, nil
	}

	now := time.Now().UTC()
	log := slices.Clone(cur.EffectLog)
	idempotencyID, ok := statusEffectIDForStep(log, cur.Generation, stepID)
	if !ok {
		idempotencyID = workflow.EffectID{
			Generation: cur.Generation,
			StepSeq:    nextStatusEffectSeq(cur),
			StepID:     stepID,
			Pos:        0,
		}
	}
	record := workflow.EffectRecord{
		ID:       idempotencyID,
		IntentAt: now,
	}
	record.CompletedAt = &now
	log = append(log, record)

	u := eff.Update
	u.EffectLog = &log
	t, prev, err := m.store.UpdateWithPrev(id, u)
	if err != nil {
		mu.Unlock()
		return Task{}, err
	}

	var (
		fireHook            bool
		prevStatus, newStat string
	)
	if m.onStatusHook != nil {
		prevStatus = string(prev)
		newStat = string(t.Status)
		fireHook = newStat != prevStatus
	}
	if fireHook {
		m.recordFiredStatus(id, newStat)
	}
	mu.Unlock()

	if fireHook {
		m.onStatusHook(id, prevStatus, newStat)
	}
	metrics.TaskUpdated()
	m.emitter.Emit(events.TaskUpdated, t.FilePath)
	return t, nil
}

func statusEffectApplied(log []workflow.EffectRecord, generation int64, stepID string) bool {
	for i := range slices.Backward(log) {
		if log[i].ID.Generation == generation && log[i].ID.StepID == stepID && log[i].CompletedAt != nil {
			return true
		}
	}
	return false
}

func statusEffectIDForStep(log []workflow.EffectRecord, generation int64, stepID string) (workflow.EffectID, bool) {
	for i := range slices.Backward(log) {
		if log[i].ID.Generation == generation && log[i].ID.StepID == stepID {
			return log[i].ID, true
		}
	}
	return workflow.EffectID{}, false
}

func nextStatusEffectSeq(t Task) int {
	maxSeq := 0
	for i := range t.EffectLog {
		if t.EffectLog[i].ID.StepSeq > maxSeq {
			maxSeq = t.EffectLog[i].ID.StepSeq
		}
	}
	if t.Workflow != nil {
		for i := range t.Workflow.EffectLog {
			if t.Workflow.EffectLog[i].ID.StepSeq > maxSeq {
				maxSeq = t.Workflow.EffectLog[i].ID.StepSeq
			}
		}
	}
	return maxSeq + 1
}

func statusEffectStepID(source string, u Update) string {
	var b strings.Builder
	writeStatusEffectField(&b, "status", statusValue(u.Status))
	writeStatusEffectField(&b, "status_reason", stringValue(u.StatusReason))
	writeStatusEffectField(&b, "pr_number", intValue(u.PRNumber))
	writeStatusEffectField(&b, "outcome", stringValue(u.Outcome))
	writeStatusEffectField(&b, "merge_commit", stringValue(u.MergeCommit))
	writeStatusEffectField(&b, "tags", tagsValue(u.Tags))
	writeStatusEffectField(&b, "blocker", blockerValue(u.Blocker))
	sum := sha256.Sum256([]byte(b.String()))
	return "external:" + sanitizeEffectSource(source) + ":" + hex.EncodeToString(sum[:6])
}

func writeStatusEffectField(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteByte('\n')
}

func sanitizeEffectSource(source string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(source) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "effect"
	}
	return strings.Trim(b.String(), "_")
}

func statusValue(v *Status) string {
	if v == nil {
		return "<unset>"
	}
	return string(*v)
}

func stringValue(v *string) string {
	if v == nil {
		return "<unset>"
	}
	return *v
}

func intValue(v *int) string {
	if v == nil {
		return "<unset>"
	}
	return strconv.Itoa(*v)
}

func tagsValue(v *[]string) string {
	if v == nil {
		return "<unset>"
	}
	return strings.Join(*v, ",")
}

func blockerValue(v *blocker.State) string {
	if v == nil {
		return "<unset>"
	}
	retryAfter := ""
	if v.RetryAfter != nil {
		retryAfter = v.RetryAfter.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{
		string(v.Kind),
		string(v.Actor),
		v.Code,
		v.NextAction,
		retryAfter,
		strconv.FormatBool(v.Exhausted),
	}, "|")
}
