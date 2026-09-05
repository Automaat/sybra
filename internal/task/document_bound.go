package task

import (
	"fmt"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/textutil"
)

const (
	// MaxStoredDocumentBytes is the hard upper bound for one primary task
	// document. Sidecars are stored separately and do not count toward it.
	MaxStoredDocumentBytes = 1 << 20 // 1 MiB

	// A run keeps enough prompt/result text for the Runs tab to remain useful,
	// while the full provider transcript remains available through LogFile.
	MaxStoredRunTextBytes = 2000
	MaxStoredAgentRuns    = 100
)

const documentTruncationSuffix = "\n... [truncated to bound task document]"

// BoundStoredDocument returns the exact Task value that may be persisted.
// Tasks already within every bound are returned unchanged. Compaction keeps
// the newest run/history records because current workflow routing reads from
// the tail, and leaves a durable receipt for the operator whenever data was
// discarded.
func BoundStoredDocument(t Task, now time.Time) (Task, error) {
	original, err := marshalTask(t, false)
	if err != nil {
		return Task{}, err
	}
	originalBytes := len(original)
	receipt := cloneDocumentCompaction(t.DocumentCompaction)
	runsChanged := boundRunHistory(&t, &receipt)
	workflowChanged := boundWorkflowText(&t, &receipt)
	if runsChanged || workflowChanged {
		stampCompaction(&t, receipt, now, originalBytes)
	}
	return shrinkStoredDocument(t, now, originalBytes)
}

func boundRunHistory(t *Task, receipt *DocumentCompaction) bool {
	if len(t.AgentRuns) == 0 {
		return false
	}
	changed := false
	t.AgentRuns = append([]AgentRun(nil), t.AgentRuns...)
	if dropped := len(t.AgentRuns) - MaxStoredAgentRuns; dropped > 0 {
		receipt.DroppedRunCostUSD += agentRunCostPrefix(t.AgentRuns, dropped)
		t.AgentRuns = slices.Delete(t.AgentRuns, 0, dropped)
		receipt.DroppedAgentRuns += dropped
		changed = true
	}
	for i := range t.AgentRuns {
		if boundString(&t.AgentRuns[i].Prompt, MaxStoredRunTextBytes) {
			receipt.TrimmedRunFields++
			changed = true
		}
		if boundString(&t.AgentRuns[i].Result, MaxStoredRunTextBytes) {
			receipt.TrimmedRunFields++
			changed = true
		}
	}
	return changed
}

// In-flight workflow outputs are normally bounded at their write sites.
// Reapplying that policy here also repairs documents written before those
// guards existed and closes less common direct-construction paths.
func boundWorkflowText(t *Task, receipt *DocumentCompaction) bool {
	if t.Workflow == nil {
		return false
	}
	changed := false
	workflow := t.Workflow.Clone()
	if workflow == nil {
		return false
	}
	t.Workflow = workflow
	for i := range workflow.StepHistory {
		changed = recordWorkflowTrim(&workflow.StepHistory[i].Output, receipt) || changed
	}
	for key, value := range workflow.Variables {
		if boundString(&value, 2000) {
			workflow.Variables[key] = value
			receipt.TrimmedWorkflow++
			changed = true
		}
	}
	for _, parent := range workflow.ParallelInflight {
		if parent == nil {
			continue
		}
		for _, child := range parent.Children {
			if child != nil {
				changed = recordWorkflowTrim(&child.Output, receipt) || changed
			}
		}
	}
	for _, parent := range workflow.BestOfNInflight {
		if parent == nil {
			continue
		}
		for _, attempt := range parent.Attempts {
			if attempt != nil {
				changed = recordWorkflowTrim(&attempt.Output, receipt) || changed
			}
		}
	}
	return changed
}

func recordWorkflowTrim(value *string, receipt *DocumentCompaction) bool {
	if !boundString(value, 4000) {
		return false
	}
	receipt.TrimmedWorkflow++
	return true
}

func shrinkStoredDocument(t Task, now time.Time, originalBytes int) (Task, error) {
	for {
		stored, marshalErr := marshalTask(t, false)
		if marshalErr != nil {
			return Task{}, marshalErr
		}
		if len(stored) <= MaxStoredDocumentBytes {
			return t, nil
		}

		receipt := cloneDocumentCompaction(t.DocumentCompaction)
		switch {
		case len(t.AgentRuns) > 0:
			drop := max(1, len(t.AgentRuns)/4)
			receipt.DroppedRunCostUSD += agentRunCostPrefix(t.AgentRuns, drop)
			t.AgentRuns = slices.Delete(t.AgentRuns, 0, drop)
			receipt.DroppedAgentRuns += drop
		case t.Workflow != nil && len(t.Workflow.StepHistory) > 0:
			drop := max(1, len(t.Workflow.StepHistory)/4)
			t.Workflow.StepHistory = slices.Delete(t.Workflow.StepHistory, 0, drop)
			receipt.TrimmedWorkflow += drop
		case len(t.EffectLog) > 0:
			drop := max(1, len(t.EffectLog)/4)
			t.EffectLog = slices.Delete(slices.Clone(t.EffectLog), 0, drop)
			receipt.TrimmedWorkflow += drop
		case len(t.Body) > 0:
			keep := len(t.Body) - (len(stored) - MaxStoredDocumentBytes) - len(documentTruncationSuffix) - 512
			t.Body = textutil.TruncateBytesTotal(t.Body, max(0, keep), documentTruncationSuffix)
			receipt.BodyTruncated = true
		default:
			return Task{}, fmt.Errorf("task document metadata is %d bytes, exceeds %d-byte maximum", len(stored), MaxStoredDocumentBytes)
		}
		stampCompaction(&t, receipt, now, originalBytes)
	}
}

func agentRunCostPrefix(runs []AgentRun, count int) float64 {
	var total float64
	for i := range min(len(runs), count) {
		total += runs[i].CostUSD
	}
	return total
}

func boundString(value *string, limit int) bool {
	bounded := textutil.TruncateBytesTotal(*value, limit, documentTruncationSuffix)
	if bounded == *value {
		return false
	}
	*value = bounded
	return true
}

func cloneDocumentCompaction(in *DocumentCompaction) DocumentCompaction {
	if in == nil {
		return DocumentCompaction{}
	}
	return *in
}

func stampCompaction(t *Task, receipt DocumentCompaction, now time.Time, originalBytes int) {
	receipt.LastCompactedAt = now.UTC()
	receipt.LargestBytesSeen = max(receipt.LargestBytesSeen, originalBytes)
	t.DocumentCompaction = &receipt
}
