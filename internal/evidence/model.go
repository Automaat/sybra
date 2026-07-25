// Package evidence defines the durable, versioned CompletionEvidence artifact
// a task accumulates as it passes deterministic gates, structured tests, and
// review — and that the workflow engine's require_evidence step consults
// before letting a task land. It is local-debug/audit data (backed by
// internal/artifact, which never scrubs at write time); anything derived from
// it that reaches a public destination must go through the same scrub path
// every other artifact-store consumer uses.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ProofType classifies how a CriterionEvidence entry was produced.
type ProofType string

const (
	// ProofDeterministicCheck is a machine-run command (verify suite, tamper
	// scan, codegen gate, focused check) with a real exit status.
	ProofDeterministicCheck ProofType = "deterministic_check"
	// ProofStructuredTest is an accepted test-runner verdict backed by the
	// structured evidence contract (readiness probe / manual probes /
	// automated checks) — see hasManualPassEvidence.
	ProofStructuredTest ProofType = "structured_test"
	// ProofReviewFinding is a review-role step completing without flipping the
	// task back into a fix_review round.
	ProofReviewFinding ProofType = "review_finding"
	// ProofManual is an explicit human attestation overriding a deterministic
	// check (e.g. the verify-blessed / tamper-blessed tags) rather than the
	// check itself passing.
	ProofManual ProofType = "manual"
)

// CriterionEvidence is one proof that a single named gate/criterion passed
// for a task at a specific revision.
type CriterionEvidence struct {
	// Criterion names the gate this proof belongs to, e.g. "verify_checks",
	// "detect_tampering", "test_runner", "review".
	Criterion    string    `json:"criterion"`
	ProofType    ProofType `json:"proofType"`
	Command      string    `json:"command,omitempty"`
	ExitStatus   int       `json:"exitStatus"`
	ResultDigest string    `json:"resultDigest,omitempty"`
	BaseRev      string    `json:"baseRev,omitempty"`
	FinalRev     string    `json:"finalRev,omitempty"`
	// Backend identifies the machine/environment that produced the proof
	// (typically a hostname) — part of the tamper-resistance story: evidence
	// carries where it was produced, not just what it says.
	Backend   string    `json:"backend,omitempty"`
	StepID    string    `json:"stepId,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Passed reports whether this proof represents a passing result.
func (c CriterionEvidence) Passed() bool { return c.ExitStatus == 0 }

// CurrentSchemaVersion is the CompletionEvidence schema version this package
// writes. Bump it, and branch on Load's parsed SchemaVersion, if the shape
// ever changes incompatibly.
const CurrentSchemaVersion = 1

// CompletionEvidence is the versioned, durable proof set bound to one task.
// It accumulates one CriterionEvidence entry per named criterion (the latest
// entry replaces any prior one for the same criterion — see Store.Append) so
// require_evidence can assert, at PR-landing time, that every required
// criterion has fresh, passing evidence for the task's current HEAD.
type CompletionEvidence struct {
	SchemaVersion int                 `json:"schemaVersion"`
	TaskID        string              `json:"taskId"`
	Criteria      []CriterionEvidence `json:"criteria,omitempty"`
	UpdatedAt     time.Time           `json:"updatedAt"`
}

// ByCriterion returns the current entry for the named criterion, if any.
func (c CompletionEvidence) ByCriterion(name string) (CriterionEvidence, bool) {
	for i := range c.Criteria {
		if c.Criteria[i].Criterion == name {
			return c.Criteria[i], true
		}
	}
	return CriterionEvidence{}, false
}

// Digest returns the lowercase-hex sha256 digest of content — used to bind a
// CriterionEvidence entry to the exact report/output it was derived from
// without embedding the (potentially large, potentially sensitive) content
// itself in the durable evidence artifact.
func Digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
