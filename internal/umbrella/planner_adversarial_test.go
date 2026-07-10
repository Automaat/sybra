package umbrella

import (
	"strings"
	"testing"
)

// TestAdversarialDuplicateAlsoNamesOmittedRef guards against a regression
// where a duplicate ref masks an omitted one: the child count stays equal to
// len(subs) even though one sub-issue is missing and another is doubled. The
// corrective retry prompt must still name both, per the task requirement to
// "feed the validate error back into the retry prompt (name the
// omitted/duplicated refs)".
func TestAdversarialDuplicateAlsoNamesOmittedRef(t *testing.T) {
	t.Parallel()
	plan := Plan{Children: []PlannedChild{
		{Ref: "o/r#1"},
		{Ref: "o/r#1"},
	}}
	s := subs("o/r#1", "o/r#2")

	err := plan.validate(s)
	if err == nil {
		t.Fatal("expected validate to fail on duplicate+omission")
	}

	corrected := correctivePrompt("original prompt", err)
	if !strings.Contains(corrected, "o/r#1") {
		t.Errorf("retry correction missing duplicated ref: %s", corrected)
	}
	if !strings.Contains(corrected, "o/r#2") {
		t.Errorf("retry correction missing omitted ref: %s", corrected)
	}
}
