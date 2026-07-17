package skillattr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	Unknown = "unknown"
	None    = "none"

	ExecutionModeUnknown     = Unknown
	ExecutionModeNone        = None
	ExecutionModeNative      = "native"
	ExecutionModeInjected    = "injected"
	ExecutionModeFallback    = "fallback"
	ExecutionModeUnavailable = "unavailable"

	ConformanceUnknown     = Unknown
	ConformanceNone        = None
	ConformanceExact       = "exact"
	ConformanceFallback    = "fallback"
	ConformanceUnavailable = "unavailable"
	// ConformanceUnverified marks a run that was delivered exact/fallback
	// but whose transcript never produced the matching conformance receipt
	// (see VerifyReceipt) — the skill was handed to the model, but nothing
	// proves it was actually followed. Downstream cohort analysis must treat
	// this the same as a failed delivery, never as ConformanceExact.
	ConformanceUnverified = "unverified"
	// ConformanceRecovered marks a run whose first attempt was
	// ConformanceUnverified and was then automatically retried once with the
	// same deterministic instructions. Recorded distinctly from a first-pass
	// ConformanceExact/ConformanceFallback run so cohort comparisons never
	// silently conflate a recovered run with a clean first-pass one.
	ConformanceRecovered = "recovered"
)

func NormalizeExecutionMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case ExecutionModeNone,
		ExecutionModeNative,
		ExecutionModeInjected,
		ExecutionModeFallback,
		ExecutionModeUnavailable:
		return strings.TrimSpace(mode)
	default:
		return ExecutionModeUnknown
	}
}

func NormalizeConformance(state string) string {
	switch strings.TrimSpace(state) {
	case ConformanceNone,
		ConformanceExact,
		ConformanceFallback,
		ConformanceUnavailable,
		ConformanceUnverified,
		ConformanceRecovered:
		return strings.TrimSpace(state)
	default:
		return ConformanceUnknown
	}
}

func ConformanceForExecutionMode(mode string) string {
	switch NormalizeExecutionMode(mode) {
	case ExecutionModeNone:
		return ConformanceNone
	case ExecutionModeNative, ExecutionModeInjected:
		return ConformanceExact
	case ExecutionModeFallback:
		return ConformanceFallback
	case ExecutionModeUnavailable:
		return ConformanceUnavailable
	default:
		return ConformanceUnknown
	}
}

func HashSourceID(sourceID string) string {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sourceID))
	return hex.EncodeToString(sum[:8])
}

// ReceiptTag names the sentinel HTML comment a mandatory workflow skill run
// must close its final response with. It doubles as the substring completion
// scans for before attempting the stricter exact-marker match, so a
// near-miss (wrong skill/hash) is still distinguishable in logs from "the
// model never attempted a receipt at all".
const ReceiptTag = "sybra-skill-receipt"

// ReceiptMarker formats the exact line a mandatory workflow skill run must
// close its final response with. sourceHash is empty for a natively-visible
// skill with no locally resolved file to hash — the marker then identifies
// the skill by name only.
func ReceiptMarker(skill, sourceHash string) string {
	if sourceHash == "" {
		return fmt.Sprintf("<!-- %s skill=%q -->", ReceiptTag, skill)
	}
	return fmt.Sprintf("<!-- %s skill=%q source=%q -->", ReceiptTag, skill, sourceHash)
}

// ReceiptInstruction builds the deterministic prompt suffix appended to every
// mandatory-skill run — native, injected, and bundled-fallback alike — so
// completion can later verify the workflow was actually followed rather than
// merely delivered. Emitting this identically regardless of delivery mode is
// what makes a retry (see VerifyReceipt) "deterministic": the instruction
// never changes across attempts.
func ReceiptInstruction(skill, sourceHash string) string {
	marker := ReceiptMarker(skill, sourceHash)
	return fmt.Sprintf(
		"Mandatory workflow skill conformance receipt: after you finish following the %q workflow above, "+
			"the LAST line of your final response must be exactly this line, verbatim, with nothing after it:\n\n%s\n\n"+
			"Omitting or altering this line means the run cannot be verified as having followed the mandatory "+
			"skill and will be treated as unconformant.",
		skill, marker,
	)
}

// FindReceipt reports whether transcript contains the receipt marker for
// skill, matching sourceHash when non-empty.
func FindReceipt(transcript, skill, sourceHash string) bool {
	if strings.TrimSpace(skill) == "" {
		return false
	}
	return strings.Contains(transcript, ReceiptMarker(skill, sourceHash))
}

// VerifyReceipt downgrades a pre-execution ConformanceExact/ConformanceFallback
// classification to ConformanceUnverified when transcript lacks the matching
// receipt marker for skill/sourceHash — i.e. the skill was delivered to the
// model (natively or injected) but nothing in its output proves it was
// actually followed. Other conformance states (none/unavailable/unknown) pass
// through unchanged: there was no delivered skill to receipt-check.
func VerifyReceipt(conformance, transcript, skill, sourceHash string) string {
	switch conformance {
	case ConformanceExact, ConformanceFallback:
		if FindReceipt(transcript, skill, sourceHash) {
			return conformance
		}
		return ConformanceUnverified
	default:
		return conformance
	}
}
