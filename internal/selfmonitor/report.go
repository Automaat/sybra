package selfmonitor

import (
	"time"

	"github.com/Automaat/sybra/internal/health"
)

// ReportSchemaVersion is bumped whenever the Report payload shape changes in
// a way that downstream consumers (GUI, CLI, cron dashboards) must know
// about.
//
// v2 replaced the coarse State/FailureReason enum and the coverage counters
// (InputsTotal/InputsAnalyzed/…) with the boolean Degraded flag plus
// AnalysisFailures. A v1 report's "partial"/"failed" state lived in a `state`
// field this shape no longer reads, so it would deserialize with
// Degraded=false and read as a clean tick — consumers must reject a version
// they don't recognize rather than trust it (see harnessevolution.Run).
const ReportSchemaVersion = 2

// Report is the full payload emitted at the end of each selfmonitor tick.
// It's also what `sybra-cli selfmonitor scan` prints and what the Wails
// `selfmonitor:report` event carries to the frontend. All fields use
// omitempty where appropriate so empty-run reports stay compact.
type Report struct {
	SchemaVersion   int                   `json:"schemaVersion"`
	GeneratedAt     time.Time             `json:"generatedAt"`
	PeriodStart     time.Time             `json:"periodStart"`
	PeriodEnd       time.Time             `json:"periodEnd"`
	HealthScore     health.Score          `json:"healthScore,omitempty"`
	Findings        []InvestigatedFinding `json:"findings"`
	Correlations    []Correlation         `json:"correlations,omitempty"`
	IssuesCreated   []int                 `json:"issuesCreated,omitempty"`
	IssuesCommented []int                 `json:"issuesCommented,omitempty"`
	ActionsTaken    []ActionRecord        `json:"actionsTaken,omitempty"`
	Suppressed      int                   `json:"suppressed"`
	FalsePositives  int                   `json:"falsePositives"`
	NeedsHuman      int                   `json:"needsHuman"`
	CostUSD         float64               `json:"costUsd"`
	DurationMS      int64                 `json:"durationMs"`
	// AnalysisFailures counts findings whose agent log could not be read or
	// parsed by the analyzer. Each such finding drops verdict and
	// provider-signal coverage, so a non-zero count means the tick produced
	// only partial evidence and is marked Degraded below.
	AnalysisFailures int `json:"analysisFailures,omitempty"`
	// Degraded marks a tick that either failed before producing findings (e.g.
	// the health report could not be read/parsed) or completed with incomplete
	// evidence (one or more per-finding log analyses failed — see
	// AnalysisFailures). Error carries the failure so LastReport() and the
	// frontend event stream reflect that self-monitor coverage is impaired
	// instead of reading as a clean, fully-analyzed tick.
	Degraded bool   `json:"degraded,omitempty"`
	Error    string `json:"error,omitempty"`
}

// InvestigatedFinding is a single health.Finding after the selfmonitor
// pipeline has distilled its log, run the judge, and optionally correlated
// it with others. LogSummary is nil for board-wide findings that have no
// associated agent log.
type InvestigatedFinding struct {
	Finding     health.Finding `json:"finding"`
	Fingerprint string         `json:"fingerprint"`
	LogSummary  *LogSummary    `json:"logSummary,omitempty"`
	Verdict     Verdict        `json:"verdict"`
	IssueNumber int            `json:"issueNumber,omitempty"`
	// AnalysisError is set when the finding's agent log was resolved but its
	// evidence is incomplete: either the analyzer failed to read/parse it at
	// all (LogSummary nil) or it parsed with oversized records dropped
	// (LogSummary set but partial — see LogSummary.TruncatedRecords). Either
	// way it flags that this finding's verdict and provider-signal coverage are
	// missing evidence rather than legitimately absent (a board-wide finding
	// with no log), so tick marks the whole report Degraded.
	AnalysisError string `json:"analysisError,omitempty"`
}

// Verdict is the structured judgment produced by the stage-1 judge LLM for a
// single finding. The synthesizer reads verdicts and the actor reads
// verdicts + categories to decide whether to act.
type Verdict struct {
	Classification  string  `json:"classification"`
	RootCause       string  `json:"rootCause,omitempty"`
	EvidenceExcerpt string  `json:"evidenceExcerpt,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
	NextAction      string  `json:"nextAction,omitempty"`
}

// Correlation is a cross-finding join discovered by the pure-Go correlator.
// Examples: all failures on the same project sharing a permission_denied
// error class; cascades where one failed impl triggered a stuck task.
type Correlation struct {
	Kind         string   `json:"kind"`
	Key          string   `json:"key"`
	Count        int      `json:"count"`
	Fingerprints []string `json:"fingerprints,omitempty"`
	Description  string   `json:"description,omitempty"`
}

// ActionRecord describes an autonomous action the actor took (or would have
// taken, when DryRun is true) in response to a confirmed finding.
type ActionRecord struct {
	Category    string    `json:"category"`
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	Reference   string    `json:"reference,omitempty"`
	DryRun      bool      `json:"dryRun"`
	TakenAt     time.Time `json:"takenAt"`
	Error       string    `json:"error,omitempty"`
}
