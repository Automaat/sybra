package selfmonitor

import (
	"time"

	"github.com/Automaat/sybra/internal/health"
)

// ReportSchemaVersion is bumped whenever the Report payload shape changes in
// a way that downstream consumers (GUI, CLI, cron dashboards) must know
// about.
const ReportSchemaVersion = 1

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
