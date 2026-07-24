// Package failureclassify holds the canonical vocabulary the workflow
// engine's gates use to describe why a check failed, plus the deterministic
// signal detectors shared across gates. verify_checks, the codegen gate, and
// route_test_result each independently decided infra-vs-code failure —
// divergent classification meant divergent retry/block/escalate outcomes for
// the same underlying signal (#2500). Consolidating the vocabulary and the
// pure output-matching detectors here gives every gate one source of truth
// to classify against.
package failureclassify

import "regexp"

// Kind identifies why a gate step failed, driving the retry/block/escalate
// decision every consumer makes on top of it.
type Kind string

const (
	// InfraFailure means the failure reflects unstable verifier
	// infrastructure (toolchain, build cache, runner crash) rather than a
	// defect in the diff or a bad test report. Consumers retry or reroute
	// instead of blocking the implementation.
	InfraFailure Kind = "infra_failure"
	// CodeFixableLint is a deterministic lint finding on a file the
	// implementation actually touched — safe to auto re-ask.
	CodeFixableLint Kind = "code_fixable_lint"
	// FrontendDeterministicFailure is a deterministic frontend check
	// failure (vitest/svelte-check) attributable to a changed frontend file.
	FrontendDeterministicFailure Kind = "frontend_deterministic_failure"
	// UnrelatedFailure means the verify suite failed only in Go package(s)
	// the diff never touched — a pre-existing failure, not this change.
	UnrelatedFailure Kind = "unrelated_failure"
	// Pass means the run produced no failure.
	Pass Kind = "pass"
	// ProductBug is a grounded test-runner FAIL report describing a real
	// defect in the implementation.
	ProductBug Kind = "product_bug"
	// AmbiguousRequirement is a grounded test-runner FAIL report describing
	// spec ambiguity rather than a clear-cut defect.
	AmbiguousRequirement Kind = "ambiguous_requirement"
	// MissingEvidence is a test-runner report that failed to ground its
	// verdict in machine-checkable evidence.
	MissingEvidence Kind = "missing_evidence"
	// ProtocolViolation is a test-runner report that violated the FAIL
	// report contract (e.g. offered fix suggestions instead of symptoms).
	ProtocolViolation Kind = "protocol_violation"
)

// String returns the Kind's wire/log value.
func (k Kind) String() string { return string(k) }

// goInfraFailurePatterns recognizes Go toolchain/build-cache instability
// (linker terminated, cache artifacts vanished) as opposed to a genuine
// implementation defect.
var goInfraFailurePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)\blink: signal: terminated\b`),
	regexp.MustCompile(`(?mi)(?:can't open import:.*go-build|go-build[\\/].*no such file or directory)`),
}

// IsGoInfraFailure reports whether output carries a signature of Go
// toolchain/build-cache instability rather than a genuine implementation
// defect.
func IsGoInfraFailure(output string) bool {
	for _, re := range goInfraFailurePatterns {
		if re.MatchString(output) {
			return true
		}
	}
	return false
}

// missingToolchainPattern recognizes a command failing because a toolchain
// executable is missing from PATH, as opposed to a genuine implementation
// defect.
var missingToolchainPattern = regexp.MustCompile(
	`(?i)command not found|executable file not found|not found in \$?PATH|[\w./-]+: not found`)

// IsMissingToolchain reports whether output shows a command failing because
// a toolchain executable is missing from PATH.
func IsMissingToolchain(output string) bool {
	return missingToolchainPattern.MatchString(output)
}
