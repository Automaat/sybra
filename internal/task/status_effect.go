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
	"github.com/Automaat/sybra/internal/workflow"
)

const maxTaskEffectLog = 200

// StatusEffect is one observer-owned task mutation recorded durably in
// Task.EffectLog before it is considered applied. ToStatus is a named field
// rather than Extra.Status for the same reason TransitionIntent splits
// ToStatus from Extra: it keeps Update.Status out of every production call
// site's composite literal outside this package, so there is exactly one
// place (Apply) that ever assigns it. See #2726.
type StatusEffect struct {
	Source   string
	ToStatus Status
	Extra    Update
	// ExpectedStatus, given, is a precondition on the task's current status —
	// plumbed straight through to TransitionIntent.ExpectedStatus. The zero
	// value (empty string) skips the check, matching the pre-existing default
	// for every caller that predates this field. Set it to the status the
	// caller already observed on the task it read, so a concurrent write
	// between that read and this call surfaces as a conflict instead of
	// silently overwriting it.
	ExpectedStatus Status
}

// ApplyStatusEffect applies a poller/watchdog/recovery-owned status mutation
// exactly once per consumed task generation and logical effect signature.
// Replays of the same Source+ToStatus+Extra after completion become a no-op
// until another task mutation advances the generation.
//
// It is a thin wrapper around Apply: the source+status+extra hash becomes the
// transition's IdempotencyKey, so external observer effects and any other
// caller of Apply share one durable effect log and one dedup rule instead of
// two independently-maintained mechanisms.
func (m *Manager) ApplyStatusEffect(id string, eff StatusEffect) (Task, error) {
	source := strings.TrimSpace(eff.Source)
	if source == "" {
		return Task{}, fmt.Errorf("apply status effect: source is required")
	}
	if eff.ToStatus == "" {
		return Task{}, fmt.Errorf("apply status effect: to_status is required")
	}
	if eff.Extra.Status != nil {
		return Task{}, fmt.Errorf("apply status effect: extra.status must be nil; set to_status instead")
	}

	var expectedStatus *Status
	if eff.ExpectedStatus != "" {
		expectedStatus = &eff.ExpectedStatus
	}
	result, err := m.Apply(TransitionIntent{
		TaskID:         id,
		ToStatus:       eff.ToStatus,
		Actor:          "effect:" + source,
		Extra:          eff.Extra,
		ExpectedStatus: expectedStatus,
		IdempotencyKey: statusEffectStepID(source, eff.ToStatus, eff.Extra),
	})
	if err != nil {
		return Task{}, err
	}
	return result.Task, nil
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

func statusEffectStepID(source string, toStatus Status, u Update) string {
	var b strings.Builder
	writeStatusEffectField(&b, "status", statusValue(&toStatus))
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
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "effect"
	}
	return out
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
	tags := slices.Clone(*v)
	slices.Sort(tags)
	return strings.Join(tags, ",")
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
