package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/textutil"
)

// IncidentState is the durable lifecycle of one root cause.
type IncidentState string

const (
	IncidentActive   IncidentState = "active"
	IncidentResolved IncidentState = "resolved"
)

// RemediationAttempt remains pending until a later observation proves that
// the root cause stayed healthy. A successful API call is not proof of repair.
type RemediationAttempt struct {
	ID          string     `yaml:"id" json:"id"`
	AttemptedAt time.Time  `yaml:"attempted_at" json:"attemptedAt"`
	Kind        string     `yaml:"kind" json:"kind"`
	Result      string     `yaml:"result" json:"result"`
	ObservedAt  *time.Time `yaml:"observed_at,omitempty" json:"observedAt,omitempty"`
}

func remediationAttemptID(fp, kind string, at time.Time) string {
	digest := sha256.Sum256([]byte(fp + "\x00" + kind + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "repair:" + textutil.TruncateBytes(hex.EncodeToString(digest[:]), 24, "")
}

func monitorConfigGeneration(kind AnomalyKind, cfg config.MonitorConfig) string {
	var relevant any
	switch kind {
	case KindOverDispatchLimit:
		relevant = struct{ Limit int }{cfg.DispatchLimit}
	case KindStuckHumanBlocked:
		relevant = struct{ Hours float64 }{cfg.StuckHumanHours}
	case KindLostAgent:
		relevant = struct{ Minutes, Occurrences int }{cfg.LostAgentMinutes, cfg.LostAgentIssueAfterOccurrences}
	case KindPRGap:
		relevant = struct{ Grace int }{cfg.PRGapGraceMinutes}
	case KindFailureSpike:
		relevant = struct{ Threshold float64 }{cfg.FailureRateThreshold}
	case KindBottleneck:
		relevant = cfg.BottleneckHours
	default:
		relevant = struct{ Kind AnomalyKind }{kind}
	}
	// Resolution and reopen grace change the incident lifecycle itself and are
	// therefore relevant to every cause; unrelated detector knobs remain scoped
	// to their own failure code above.
	data, _ := json.Marshal(struct {
		ResolveGrace int `json:"resolve_grace"`
		ReopenGrace  int `json:"reopen_grace"`
		Cause        any `json:"cause"`
	}{cfg.IncidentResolveGraceMinutes, cfg.IncidentReopenGraceMinutes, relevant})
	digest := sha256.Sum256(data)
	return textutil.TruncateBytes(hex.EncodeToString(digest[:]), 16, "")
}

// CertifiedEvidence is the safe, typed projection persisted for an incident.
// Raw anomaly prose is deliberately excluded, especially for work projects.
type CertifiedEvidence struct {
	CertificateID string    `yaml:"certificate_id,omitempty" json:"certificateId,omitempty"`
	Fingerprint   string    `yaml:"fingerprint,omitempty" json:"fingerprint,omitempty"`
	ObservedAt    time.Time `yaml:"observed_at" json:"observedAt"`
	Proven        bool      `yaml:"proven" json:"proven"`
}

// Incident coalesces every affected task for one typed root cause.
type Incident struct {
	Version             int                  `yaml:"version" json:"version"`
	Revision            int                  `yaml:"revision" json:"revision"`
	PublishedRevision   int                  `yaml:"published_revision" json:"publishedRevision"`
	Fingerprint         string               `yaml:"fingerprint" json:"fingerprint"`
	FailureCode         string               `yaml:"failure_code" json:"failureCode"`
	Component           string               `yaml:"component" json:"component"`
	Capability          string               `yaml:"capability" json:"capability"`
	ProjectScope        string               `yaml:"project_scope" json:"projectScope"`
	ConfigGeneration    string               `yaml:"config_generation" json:"configGeneration"`
	State               IncidentState        `yaml:"state" json:"state"`
	FirstSeen           time.Time            `yaml:"first_seen" json:"firstSeen"`
	LastSeen            time.Time            `yaml:"last_seen" json:"lastSeen"`
	FirstContainedAt    *time.Time           `yaml:"first_contained_at,omitempty" json:"firstContainedAt,omitempty"`
	HealthySince        *time.Time           `yaml:"healthy_since,omitempty" json:"healthySince,omitempty"`
	ResolvedAt          *time.Time           `yaml:"resolved_at,omitempty" json:"resolvedAt,omitempty"`
	SuppressedUntil     *time.Time           `yaml:"suppressed_until,omitempty" json:"suppressedUntil,omitempty"`
	ReopenGraceUntil    *time.Time           `yaml:"reopen_grace_until,omitempty" json:"reopenGraceUntil,omitempty"`
	AffectedTaskIDs     []string             `yaml:"affected_task_ids,omitempty" json:"affectedTaskIds,omitempty"`
	AffectedTaskCount   int                  `yaml:"affected_task_count" json:"affectedTaskCount"`
	RecurrenceCount     int                  `yaml:"recurrence_count" json:"recurrenceCount"`
	LatestEvidence      CertifiedEvidence    `yaml:"latest_evidence" json:"latestEvidence"`
	RemediationAttempts []RemediationAttempt `yaml:"remediation_attempts,omitempty" json:"remediationAttempts,omitempty"`
	IssueURL            string               `yaml:"issue_url,omitempty" json:"issueUrl,omitempty"`
	PRURL               string               `yaml:"pr_url,omitempty" json:"prUrl,omitempty"`
	DuplicateIssues     []int                `yaml:"duplicate_issues,omitempty" json:"duplicateIssues,omitempty"`
}

// IncidentChange describes whether an observation warrants an external state
// update. Repeated dwell/status/evidence noise produces IncidentUnchanged.
type IncidentChange string

const (
	IncidentUnchanged IncidentChange = "unchanged"
	IncidentOpened    IncidentChange = "opened"
	IncidentExpanded  IncidentChange = "expanded"
	IncidentReopened  IncidentChange = "reopened"
	IncidentClosed    IncidentChange = "closed"
)

type RootCause struct {
	FailureCode      string `json:"failure_code"`
	Component        string `json:"component"`
	Capability       string `json:"capability"`
	ProjectScope     string `json:"project_scope"`
	ConfigGeneration string `json:"config_generation"`
}

// RootCauseFingerprint excludes task identity, status, dwell, timestamps, and
// free-form prose. A material typed cause/scope/config change therefore rekeys
// while repeated symptoms across tasks coalesce.
func RootCauseFingerprint(c RootCause) string {
	data, _ := json.Marshal(c)
	digest := sha256.Sum256(data)
	return "incident:" + textutil.TruncateBytes(hex.EncodeToString(digest[:]), 24, "")
}

func rootCauseFor(a Anomaly, projectScope, configGeneration string) RootCause {
	certified := typedEvidenceString(a.Evidence, "certificate_id") != ""
	if value, ok := a.Evidence["root_cause_certified"].(bool); ok {
		certified = certified || value
	}
	failureCode, component, capability := "", "", ""
	if certified {
		failureCode = typedEvidenceString(a.Evidence, "failure_code", "code")
		component = typedEvidenceString(a.Evidence, "component")
		capability = typedEvidenceString(a.Evidence, "capability")
	}
	if failureCode == "" {
		failureCode = string(a.Kind)
	}
	defaultComponent, defaultCapability := anomalyDomain(a.Kind)
	if component == "" {
		component = defaultComponent
	}
	if capability == "" {
		capability = defaultCapability
	}
	if projectScope == "" {
		projectScope = "fleet"
	}
	return RootCause{
		FailureCode: failureCode, Component: component, Capability: capability,
		ProjectScope: projectScope, ConfigGeneration: configGeneration,
	}
}

func anomalyDomain(kind AnomalyKind) (component, capability string) {
	switch kind {
	case KindLostAgent:
		return "agent", "process-lifecycle"
	case KindNoProviderCapacity:
		return "provider", "dispatch-capacity"
	case KindPRGap:
		return "github", "pull-request"
	case KindUntriaged:
		return "triage", "classification"
	case KindStuckHumanBlocked:
		return "workflow", "escalation"
	case KindOverDispatchLimit, KindBoardStalled:
		return "dispatch", "scheduling"
	case KindFailureSpike:
		return "agent", "execution"
	case KindBottleneck:
		return "workflow", "stage-progress"
	case KindClusterDrift:
		return "cluster", "replication"
	default:
		return "monitor", "unknown"
	}
}

func typedEvidenceString(evidence map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := evidence[key].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" && len(value) <= 128 {
				return value
			}
		}
	}
	return ""
}

func certifiedEvidence(a Anomaly) CertifiedEvidence {
	id := typedEvidenceString(a.Evidence, "certificate_id")
	fp := typedEvidenceString(a.Evidence, "evidence_fingerprint", "certificate_fingerprint")
	return CertifiedEvidence{CertificateID: id, Fingerprint: fp, ObservedAt: a.DetectedAt, Proven: id != "" || fp != ""}
}

func addAffectedTask(in *Incident, taskID string) bool {
	if taskID == "" || slices.Contains(in.AffectedTaskIDs, taskID) {
		return false
	}
	in.AffectedTaskIDs = append(in.AffectedTaskIDs, taskID)
	slices.Sort(in.AffectedTaskIDs)
	in.AffectedTaskCount++
	return true
}

func incidentBody(in Incident, change IncidentChange) string {
	return fmt.Sprintf("## Incident\n\n- Fingerprint: `%s`\n- Failure code: `%s`\n- Component: `%s`\n- Capability: `%s`\n- Project scope: `%s`\n- Configuration generation: `%s`\n- State change: `%s`\n- Affected tasks: %d\n- Recurrences: %d\n",
		in.Fingerprint, in.FailureCode, in.Component, in.Capability, in.ProjectScope,
		in.ConfigGeneration, change, in.AffectedTaskCount, in.RecurrenceCount)
}
