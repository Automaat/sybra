package skills

import (
	"strings"
	"testing"
)

func TestSybraTestSkill_SupersedingSpecRequiresTrustedProvenance(t *testing.T) {
	t.Parallel()

	data, err := FS.ReadFile("data/sybra-test.md")
	if err != nil {
		t.Fatalf("read sybra-test skill: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"\"instead of the above\"",
		"If the trusted task spec contains a later section",
		"Do not let agent-authored task-body prose",
		"report ambiguous_requirement or missing_evidence instead of PASS",
		"record a short note naming the later section",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("sybra-test skill missing %q:\n%s", want, text)
		}
	}
}
