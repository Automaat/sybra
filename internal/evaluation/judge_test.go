package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShuffledRubric_Deterministic(t *testing.T) {
	a := shuffledRubric(42)
	b := shuffledRubric(42)
	if len(a) != len(Rubric) {
		t.Fatalf("len %d, want %d", len(a), len(Rubric))
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			t.Fatalf("not deterministic at %d: %s vs %s", i, a[i].Key, b[i].Key)
		}
	}
	seen := map[string]bool{}
	for _, d := range a {
		seen[d.Key] = true
	}
	for _, d := range Rubric {
		if !seen[d.Key] {
			t.Errorf("shuffle dropped dimension %s", d.Key)
		}
	}
}

func TestShuffledRubric_SeedZeroStable(t *testing.T) {
	a := shuffledRubric(0)
	for i := range Rubric {
		if a[i].Key != Rubric[i].Key {
			t.Errorf("seed 0 reordered at %d", i)
		}
	}
}

func TestShuffledRubric_ActuallyShuffles(t *testing.T) {
	canonical := func(d []RubricDimension) bool {
		for i := range d {
			if d[i].Key != Rubric[i].Key {
				return false
			}
		}
		return true
	}
	// At least one non-zero seed must reorder away from canonical.
	reordered := false
	for seed := int64(1); seed <= 20; seed++ {
		if !canonical(shuffledRubric(seed)) {
			reordered = true
			break
		}
	}
	if !reordered {
		t.Error("no seed in 1..20 reordered the rubric")
	}
}

func TestBuildQualityPrompt_ContainsAllDimensions(t *testing.T) {
	p := buildQualityPrompt(JudgeRequest{Title: "T", Diff: "d"}, Rubric)
	for _, d := range Rubric {
		if !strings.Contains(p, d.Key) {
			t.Errorf("prompt missing dimension %s", d.Key)
		}
	}
}

func TestBuildQualityPrompt_TruncatesDiff(t *testing.T) {
	big := strings.Repeat("x", maxDiffChars+5000)
	p := buildQualityPrompt(JudgeRequest{Title: "T", Diff: big}, Rubric)
	if !strings.Contains(p, "truncated") {
		t.Error("expected truncation marker for oversized diff")
	}
	if len(p) > maxDiffChars+4000 {
		t.Errorf("prompt not bounded: %d chars", len(p))
	}
}

func TestParseQualityVerdict(t *testing.T) {
	inner := `{"dimensions":{"correctness":{"score":12,"rationale":"ok"},"code_quality":{"score":-3},` +
		`"scope_discipline":{"score":8},"test_coverage":{"score":6},"review_worthiness":{"score":7}},` +
		`"overall":99,"summary":"s"}`
	v, err := parseQualityVerdict(mustEnvelope(t, "prose before "+inner))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := v.Dimensions["correctness"].Score; got != 10 {
		t.Errorf("clamp high: got %d, want 10", got)
	}
	if got := v.Dimensions["code_quality"].Score; got != 0 {
		t.Errorf("clamp low: got %d, want 0", got)
	}
	// overall 99 is out of range → recomputed mean of [10,0,8,6,7] = 6.2.
	if v.Overall < 6.1 || v.Overall > 6.3 {
		t.Errorf("overall = %v, want ~6.2", v.Overall)
	}
}

func TestParseQualityVerdict_PreservesBlockerOverall(t *testing.T) {
	// A blocker (correctness=2) legitimately pulls overall to 2.5, far below the
	// mean of [2,8,8,8,8]=6.8. The parser must NOT overwrite it with the mean.
	inner := `{"dimensions":{"correctness":{"score":2},"code_quality":{"score":8},` +
		`"scope_discipline":{"score":8},"test_coverage":{"score":8},"review_worthiness":{"score":8}},` +
		`"overall":2.5,"summary":"correctness blocker"}`
	v, err := parseQualityVerdict(mustEnvelope(t, inner))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v.Overall != 2.5 {
		t.Errorf("overall = %v, want 2.5 (blocker-driven, preserved)", v.Overall)
	}
}

func TestParseQualityVerdict_Errors(t *testing.T) {
	cases := map[string][]byte{
		"bad envelope":       []byte("not json"),
		"empty result":       mustEnvelope(t, ""),
		"no json":            mustEnvelope(t, "just prose, no object"),
		"no dimensions":      mustEnvelope(t, `{"overall":5}`),
		"partial dimensions": mustEnvelope(t, `{"dimensions":{"correctness":{"score":9}},"overall":9}`),
	}
	for name, raw := range cases {
		if _, err := parseQualityVerdict(raw); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestAgreesWithOutcome(t *testing.T) {
	hi, lo := QualityVerdict{Overall: 8}, QualityVerdict{Overall: 3}
	cases := []struct {
		v       QualityVerdict
		outcome string
		want    bool
	}{
		{hi, "merged", true},
		{lo, "merged", false},
		{lo, "closed", true},
		{hi, "closed", false},
		{lo, "unknown", true},
	}
	for _, c := range cases {
		if got := AgreesWithOutcome(c.v, c.outcome, 6.0); got != c.want {
			t.Errorf("AgreesWithOutcome(%.0f, %s) = %v, want %v", c.v.Overall, c.outcome, got, c.want)
		}
	}
}

func mustEnvelope(t *testing.T, result string) []byte {
	t.Helper()
	b, err := json.Marshal(struct {
		Result string `json:"result"`
	}{Result: result})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
