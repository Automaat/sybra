package github

import (
	"fmt"
	"strings"
	"testing"
)

// jsonExecer returns a canned JSON body for every request, regardless of args.
type jsonExecer struct {
	body string
	err  error
}

func (j *jsonExecer) run(args ...string) ([]byte, error) {
	if j.err != nil {
		return nil, j.err
	}
	return []byte(j.body), nil
}

func checkRunsJSON(runs ...string) string {
	var b strings.Builder
	b.WriteString("{\"check_runs\":[")
	for i, r := range runs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(r)
	}
	b.WriteString("]}")
	return b.String()
}

func checkRun(name, status, conclusion string) string {
	return fmt.Sprintf(`{"name":%q,"status":%q,"conclusion":%q}`, name, status, conclusion)
}

func TestClassifyCIFlakiness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		threshold  float64
		wantFlaky  bool
		wantChecks []string
	}{
		{
			name: "single check failed then passed above threshold",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
			),
			threshold:  0.75,
			wantFlaky:  true,
			wantChecks: []string{"unit-tests"},
		},
		{
			name: "check fails every attempt is deterministic",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "failure"),
			),
			threshold: 0.75,
			wantFlaky: false,
		},
		{
			name: "success rate below threshold is deterministic",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
			),
			threshold: 0.75,
			wantFlaky: false,
		},
		{
			name: "one flaky and one deterministic check is not all-flaky",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("e2e-tests", "completed", "failure"),
				checkRun("e2e-tests", "completed", "failure"),
			),
			threshold: 0.75,
			wantFlaky: false,
		},
		{
			name: "multiple flaky checks all classified",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("e2e-tests", "completed", "failure"),
				checkRun("e2e-tests", "completed", "success"),
				checkRun("e2e-tests", "completed", "success"),
				checkRun("e2e-tests", "completed", "success"),
			),
			threshold:  0.75,
			wantFlaky:  true,
			wantChecks: []string{"e2e-tests", "unit-tests"},
		},
		{
			name:      "no failing checks is not flaky",
			body:      checkRunsJSON(checkRun("unit-tests", "completed", "success")),
			threshold: 0.75,
			wantFlaky: false,
		},
		{
			name: "non-gating check is ignored",
			body: checkRunsJSON(
				checkRun("codecov/patch", "completed", "failure"),
				checkRun("codecov/patch", "completed", "failure"),
			),
			threshold: 0.75,
			wantFlaky: false,
		},
		{
			name: "pending attempts are ignored",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "in_progress", ""),
			),
			threshold:  0.75,
			wantFlaky:  true,
			wantChecks: []string{"unit-tests"},
		},
		{
			name: "exact threshold boundary counts as flaky",
			body: checkRunsJSON(
				checkRun("unit-tests", "completed", "failure"),
				checkRun("unit-tests", "completed", "success"),
				checkRun("unit-tests", "completed", "success"),
			),
			threshold:  2.0 / 3.0,
			wantFlaky:  true,
			wantChecks: []string{"unit-tests"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := &jsonExecer{body: tt.body}
			allFlaky, checks, err := classifyCIFlakinessWith(e, "o/r", "sha123", tt.threshold)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allFlaky != tt.wantFlaky {
				t.Errorf("allFlaky = %v, want %v", allFlaky, tt.wantFlaky)
			}
			if !stringSlicesEqual(checks, tt.wantChecks) {
				t.Errorf("flakyChecks = %v, want %v", checks, tt.wantChecks)
			}
		})
	}
}

func TestClassifyCIFlakiness_InvalidInput(t *testing.T) {
	t.Parallel()
	e := &jsonExecer{body: checkRunsJSON()}
	if _, _, err := classifyCIFlakinessWith(e, "not-a-repo", "sha", 0.75); err == nil {
		t.Error("want error for malformed repo")
	}
	if _, _, err := classifyCIFlakinessWith(e, "o/r", "", 0.75); err == nil {
		t.Error("want error for empty sha")
	}
}

func TestClassifyCIFlakiness_FetchError(t *testing.T) {
	t.Parallel()
	e := &jsonExecer{err: fmt.Errorf("network unreachable")}
	allFlaky, checks, err := classifyCIFlakinessWith(e, "o/r", "sha123", 0.75)
	if err == nil {
		t.Fatal("want error on fetch failure")
	}
	if allFlaky {
		t.Error("allFlaky should be false on fetch error (fail closed)")
	}
	if checks != nil {
		t.Errorf("flakyChecks = %v, want nil on fetch error", checks)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
