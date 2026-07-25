package evidence

import "testing"

func TestDigest_DeterministicAndSensitiveToContent(t *testing.T) {
	a := Digest("hello")
	b := Digest("hello")
	c := Digest("hello ")
	if a != b {
		t.Fatalf("Digest not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("Digest did not change for different content")
	}
	if len(a) != 64 {
		t.Fatalf("Digest length = %d, want 64 (hex sha256)", len(a))
	}
}

func TestCriterionEvidence_Passed(t *testing.T) {
	tests := []struct {
		name string
		exit int
		want bool
	}{
		{"zero exit passes", 0, true},
		{"nonzero exit fails", 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := CriterionEvidence{ExitStatus: tc.exit}
			if got := c.Passed(); got != tc.want {
				t.Errorf("Passed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompletionEvidence_ByCriterion(t *testing.T) {
	ce := CompletionEvidence{
		Criteria: []CriterionEvidence{
			{Criterion: "verify_checks", ExitStatus: 0},
			{Criterion: "detect_tampering", ExitStatus: 1},
		},
	}

	if got, ok := ce.ByCriterion("verify_checks"); !ok || got.ExitStatus != 0 {
		t.Errorf("ByCriterion(verify_checks) = %+v, %v", got, ok)
	}
	if got, ok := ce.ByCriterion("detect_tampering"); !ok || got.ExitStatus != 1 {
		t.Errorf("ByCriterion(detect_tampering) = %+v, %v", got, ok)
	}
	if _, ok := ce.ByCriterion("missing"); ok {
		t.Errorf("ByCriterion(missing) reported found for an absent criterion")
	}
}
