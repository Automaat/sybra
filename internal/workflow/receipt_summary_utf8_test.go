package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// These three cap arbitrary tool and test-runner output, where a malformed byte
// can sit anywhere in the text rather than only at the cut. Aligning the cut is
// not enough for them: trimReceiptSummary feeds a persisted YAML status_reason,
// and the capTextBlock pair feeds the task body and agent prompts, which reach
// the frontend as JSON.
func TestOutputCapsRepairInvalidBytesBeforeTheCut(t *testing.T) {
	dirty := strings.Repeat("ok ", 30) + "\xff\xfe" + strings.Repeat("tail … ", 900)

	for name, got := range map[string]string{
		"trimReceiptSummary":   trimReceiptSummary(dirty),
		"singleLineDiagnostic": singleLineDiagnostic(nil, dirty),
		"capLedgerRepro":       capLedgerRepro(dirty),
		"capPriorDiffStat":     capPriorAttemptDiffStat(dirty),
		"capPriorAttemptText":  capPriorAttemptText(dirty),
	} {
		if !utf8.ValidString(got) {
			t.Errorf("%s returned invalid UTF-8: %q", name, got)
		}
	}
}

// trimReceiptSummary's result is persisted as YAML frontmatter, so an invalid
// byte anywhere in it turns the whole reason into an unreadable !!binary block.
func TestReceiptSummaryMarshalsAsPlainYAML(t *testing.T) {
	// Both branches matter: a malformed byte corrupts the frontmatter whether
	// or not the line was long enough to be cut.
	for name, detail := range map[string]string{
		"truncated": "## Test Failures\nObserved output: assertion failed caf\xe9 mismatch \xff\xfe " +
			strings.Repeat("expected vs actual … ", 20),
		"under the cut": "## Test Failures\nObserved output: assertion failed caf\xe9 \xff\xfe mismatch",
	} {
		t.Run(name, func(t *testing.T) {
			summary := trimReceiptSummary(detail)
			if !utf8.ValidString(summary) {
				t.Fatalf("receipt summary is not valid UTF-8: %q", summary)
			}

			data, err := yaml.Marshal(map[string]string{"status_reason": summary})
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			if strings.Contains(string(data), "!!binary") {
				t.Fatalf("receipt summary marshalled as a binary block:\n%s", data)
			}
		})
	}
}

// The pre-refactor helper capped content at 157 bytes and only truncated past
// 160, so a string between the two came back whole. Persisted text must not
// churn on a refactor.
func TestReceiptSummaryKeepsItsOriginalLimits(t *testing.T) {
	tests := []struct {
		name    string
		length  int
		wantLen int
	}{
		{name: "at the truncation threshold", length: 160, wantLen: 160},
		{name: "one byte over", length: 161, wantLen: 160},
		{name: "far over", length: 4000, wantLen: 160},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimReceiptSummary(strings.Repeat("a", tt.length))
			if len(got) != tt.wantLen {
				t.Errorf("len(trimReceiptSummary(%d bytes)) = %d, want %d: %q",
					tt.length, len(got), tt.wantLen, got)
			}
		})
	}
}
