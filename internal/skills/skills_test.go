package skills_test

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/skills"
)

func TestSybraTriageDoesNotEscalateClassifierFailures(t *testing.T) {
	t.Parallel()

	data, err := skills.FS.ReadFile("data/sybra-triage.md")
	if err != nil {
		t.Fatalf("read sybra-triage skill: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{
		"--status human-required",
		`--status-reason "triage failed"`,
		"classify` returns an error, flag the task",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sybra-triage must not tell agents to escalate classifier failures via %q", forbidden)
		}
	}
	if !strings.Contains(text, "retryable `status_reason`") {
		t.Fatal("sybra-triage should explain that classifier failures stay retryable")
	}
}
