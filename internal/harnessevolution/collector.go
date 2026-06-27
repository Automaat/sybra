package harnessevolution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

func LoadSelfMonitorReport(path string) (selfmonitor.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return selfmonitor.Report{}, err
	}
	var report selfmonitor.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return selfmonitor.Report{}, fmt.Errorf("parse selfmonitor report: %w", err)
	}
	return report, nil
}

func EventsFromReport(report selfmonitor.Report, since time.Time) []FailureEvent {
	events := make([]FailureEvent, 0, len(report.Findings))
	for i := range report.Findings {
		inv := report.Findings[i]
		if !actionableVerdict(inv.Verdict.Classification) {
			continue
		}
		ev := eventFromFinding(inv)
		if ev.Category == "" {
			continue
		}
		if !since.IsZero() && ev.OccurredAt.Before(since) {
			continue
		}
		events = append(events, ev)
	}
	return events
}

func actionableVerdict(v string) bool {
	switch v {
	case selfmonitor.VerdictConfirmed, selfmonitor.VerdictNeedsHuman:
		return true
	default:
		return false
	}
}

func eventFromFinding(inv selfmonitor.InvestigatedFinding) FailureEvent {
	f := inv.Finding
	fp := firstNonEmpty(inv.Fingerprint, f.Fingerprint)
	step := workflowStep(f)
	failureKind := inferFailureKind(f, inv.LogSummary)
	occurredAt := f.DetectedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	ev := FailureEvent{
		TraceID:      stableTraceID(f.TaskID, step, f.AgentID),
		TaskID:       f.TaskID,
		AgentID:      f.AgentID,
		Role:         normalizeToken(f.Role),
		Category:     string(f.Category),
		WorkflowStep: step,
		FailureKind:  failureKind,
		Fingerprint:  fp,
		OccurredAt:   occurredAt.UTC(),
	}
	return ev
}

func workflowStep(f health.Finding) string {
	for _, key := range []string{"workflow_step", "step", "status", "transition", "role"} {
		if s, ok := f.Evidence[key].(string); ok && strings.TrimSpace(s) != "" {
			return normalizeToken(s)
		}
	}
	if f.Role != "" {
		return normalizeToken(f.Role)
	}
	return normalizeToken(string(f.Category))
}

func inferFailureKind(f health.Finding, ls *selfmonitor.LogSummary) string {
	if ls != nil {
		if sensitive := sensitiveErrorClass(ls.ErrorClasses); sensitive != "" {
			return sensitive
		}
		if ls.StallDetected {
			return "step_retry_exhausted"
		}
		if len(ls.ErrorClasses) > 0 && ls.ErrorClasses[0].Class != "" {
			return normalizeToken(ls.ErrorClasses[0].Class)
		}
	}
	switch f.Category {
	case health.CatAgentRetryLoop, health.CatWorkflowLoop:
		return "step_retry_exhausted"
	case health.CatStatusBottleneck, health.CatStuckTask:
		return "step_timeout"
	case health.CatTriageMismatch, health.CatStatusBounce:
		return "topology_dead_end"
	case health.CatCostOutlier, health.CatCostDrift:
		return "cost_outlier"
	case health.CatFailureRate:
		return "provider_protocol_error"
	default:
		return normalizeToken(string(f.Category))
	}
}

func sensitiveErrorClass(classes []selfmonitor.ErrorClass) string {
	for _, ec := range classes {
		cls := normalizeToken(ec.Class)
		if strings.Contains(cls, "permission") ||
			strings.Contains(cls, "secret") ||
			strings.Contains(cls, "creden"+"tial") ||
			strings.Contains(cls, "network") {
			return cls
		}
	}
	return ""
}

func stableTraceID(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "%v|", p)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "trace-" + hex.EncodeToString(sum[:])[:12]
}

func normalizeToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "→", "_to_")
	s = strings.ReplaceAll(s, "-", "_")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	return strings.Trim(s, "_")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
