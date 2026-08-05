package workflow

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/textutil"
)

// skillReceiptExhaustionSummary condenses the last failed skill output into a
// short verdict string for human-required receipt-exhaustion reasons.
func skillReceiptExhaustionSummary(output string) string {
	report := currentTestFailureReport(output, "", nil, "")
	outcome, detail := receiptSummaryOutcomeAndDetail(output, report)
	if detail == "" {
		detail = firstReceiptSummaryDetail(report,
			"observed output", "actual output", "verbatim output", "actual behaviour",
			"actual behavior", "observed behaviour", "observed behavior", "output")
	}
	if detail == "" {
		detail = firstReceiptSummaryDetail(report, "code evidence")
	}
	if detail == "" {
		detail = firstReceiptSummaryDetail(report, "expected", "requirement tested")
	}
	switch {
	case outcome != "" && detail != "":
		return outcome + ": " + detail
	case outcome != "":
		return outcome
	default:
		return detail
	}
}

func receiptSummaryOutcomeAndDetail(output, report string) (outcome, detail string) {
	if parsed, ok := parseStructuredTestOutput(output); ok {
		outcome = normalizeTestOutcome(parsed.Outcome)
	}
	testFailuresSummaryHeading := receiptSummaryLine(testFailuresHeading)
	for _, line := range reportScanLines(report) {
		clean := receiptSummaryLine(line)
		if clean == "" || strings.EqualFold(clean, testFailuresSummaryHeading) {
			continue
		}
		if field, value, ok := strings.Cut(clean, ":"); ok {
			field = strings.ToLower(strings.TrimSpace(field))
			switch field {
			case "classification", "class", "type", "outcome":
				reportOutcome := normalizeTestOutcome(value)
				if reportOutcome == "" {
					continue
				}
				if outcome == "" {
					outcome = reportOutcome
				}
				reportDetail := trimReceiptSummary(trimOutcomePrefix(value, reportOutcome))
				if reportOutcome != outcome {
					continue
				}
				if detail == "" {
					detail = reportDetail
				}
				if outcome != "" && detail != "" {
					return outcome, detail
				}
			}
		}
		if reportOutcome, reportDetail, ok := receiptSummaryHeading(line); ok {
			if outcome == "" {
				outcome = reportOutcome
			}
			if reportOutcome != outcome {
				continue
			}
			if detail == "" {
				detail = reportDetail
			}
			if outcome != "" && detail != "" {
				return outcome, detail
			}
		}
	}
	return outcome, detail
}

func firstReceiptSummaryDetail(report string, labels ...string) string {
	if strings.TrimSpace(report) == "" {
		return ""
	}
	labelSet := map[string]struct{}{}
	for _, label := range labels {
		labelSet[strings.ToLower(label)] = struct{}{}
	}
	lines := strings.Split(report, "\n")
	inFence := false
	for i := range lines {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" || strings.HasPrefix(lines[i], "    ") || strings.HasPrefix(lines[i], "\t") {
			continue
		}
		field, value, ok := receiptSummaryLabelLine(lines[i])
		if !ok {
			continue
		}
		if _, want := labelSet[field]; !want {
			continue
		}
		if detail := trimReceiptSummary(value); detail != "" {
			return detail
		}
		if detail := receiptSummaryContinuation(lines[i+1:]); detail != "" {
			return detail
		}
	}
	return ""
}

func receiptSummaryLabelLine(line string) (field, value string, ok bool) {
	clean := receiptSummaryLine(line)
	if clean == "" {
		return "", "", false
	}
	field, value, ok = strings.Cut(clean, ":")
	if !ok {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(field)), strings.TrimSpace(value), true
}

func receiptSummaryContinuation(lines []string) string {
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		clean := receiptSummaryLine(trimmed)
		if clean == "" {
			if !inFence {
				continue
			}
			continue
		}
		if !inFence {
			if _, _, ok := receiptSummaryLabelLine(trimmed); ok {
				return ""
			}
		}
		return trimReceiptSummary(clean)
	}
	return ""
}

func receiptSummaryHeading(line string) (outcome, detail string, ok bool) {
	clean := receiptSummaryLine(line)
	if clean == "" {
		return "", "", false
	}
	for _, outcome = range []string{
		testOutcomeProductBug,
		testOutcomeAmbiguousRequirement,
		testOutcomeInfraFailure,
		testOutcomeMissingEvidence,
		testOutcomeProtocolViolation,
		testOutcomePass,
	} {
		if detail = trimOutcomePrefix(clean, outcome); detail != "" {
			return outcome, detail, true
		}
		if strings.EqualFold(clean, outcome) {
			return outcome, "", true
		}
	}
	return "", "", false
}

func trimOutcomePrefix(text, outcome string) string {
	clean := receiptSummaryLine(text)
	if clean == "" {
		return ""
	}
	lower := strings.ToLower(clean)
	for _, prefix := range receiptSummaryOutcomePrefixes(outcome) {
		if !strings.HasPrefix(lower, prefix) || !hasReceiptOutcomePrefixBoundary(lower, len(prefix)) {
			continue
		}
		rest := strings.TrimSpace(clean[len(prefix):])
		rest = strings.TrimLeft(rest, " .;,:-")
		return rest
	}
	return ""
}

func hasReceiptOutcomePrefixBoundary(s string, prefixLen int) bool {
	if len(s) == prefixLen {
		return true
	}
	next, _ := utf8.DecodeRuneInString(s[prefixLen:])
	return !unicode.IsLetter(next) && !unicode.IsDigit(next)
}

func receiptSummaryOutcomePrefixes(outcome string) []string {
	prefixes := []string{outcome, strings.ReplaceAll(outcome, "_", " "), strings.ReplaceAll(outcome, "_", "-")}
	switch outcome {
	case testOutcomeProductBug:
		prefixes = append(prefixes, "bug", "product failure", "product-failure")
	case testOutcomeInfraFailure:
		prefixes = append(prefixes, "infra", "infrastructure failure", "infrastructure-failure")
	case testOutcomeMissingEvidence:
		prefixes = append(prefixes, "no evidence", "missing evidence")
	case testOutcomeAmbiguousRequirement:
		prefixes = append(prefixes, "ambiguous requirement", "ambiguous")
	case testOutcomeProtocolViolation:
		prefixes = append(prefixes, "protocol violation", "test protocol violation")
	}
	return prefixes
}

func receiptSummaryLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "#>*- \t")
	line = strings.Trim(line, "`*_ \t")
	return strings.TrimSpace(line)
}

func trimReceiptSummary(s string) string {
	s = receiptSummaryLine(s)
	if s == "" {
		return ""
	}
	// Sanitize whether or not the cut fires: this reaches a persisted YAML
	// status_reason, so a short line carrying a malformed byte serializes as
	// an unreadable !!binary block just as a truncated one does.
	s = strings.ToValidUTF8(strings.Join(strings.Fields(s), " "), "\uFFFD")
	if !isUsefulReceiptDetail(s) {
		return ""
	}
	if len(s) <= 160 {
		return s
	}
	return textutil.TruncateBytesTrimmed(s, 157, "...")
}

func isUsefulReceiptDetail(s string) bool {
	switch strings.ToLower(strings.Trim(s, " .;:-")) {
	case "", "n/a", "na", "none", "nothing", "unknown":
		return false
	default:
		return true
	}
}
