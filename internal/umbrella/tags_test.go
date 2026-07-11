package umbrella

import (
	"reflect"
	"testing"
	"time"
)

func TestInheritableLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"keeps plain labels", []string{"backend", "tech-debt"}, []string{"backend", "tech-debt"}},
		{"drops exact control tags", []string{"review", "blocked", "handoff", "umbrella", GatedTag, "sybra-bug", "scrubbed", "keep"}, []string{"keep"}},
		{"drops control families", []string{"handoff-review", "umbrella-max-parallel:5", "monitor:loop", "ok"}, []string{"ok"}},
		{"empty in empty out", nil, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := InheritableLabels(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("InheritableLabels(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestMaxParallelTag(t *testing.T) {
	t.Parallel()
	if got := MaxParallelTag(5); got != "umbrella-max-parallel:5" {
		t.Errorf("MaxParallelTag(5) = %q", got)
	}
}

func TestParseMaxParallel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		want int
	}{
		{"present", []string{"umbrella", "umbrella-max-parallel:3"}, 3},
		{"absent defaults", []string{"umbrella"}, DefaultMaxParallel},
		{"malformed defaults", []string{"umbrella-max-parallel:abc"}, DefaultMaxParallel},
		{"zero defaults", []string{"umbrella-max-parallel:0"}, DefaultMaxParallel},
		{"nil defaults", nil, DefaultMaxParallel},
	}
	for _, c := range cases {
		if got := ParseMaxParallel(c.tags); got != c.want {
			t.Errorf("%s: ParseMaxParallel(%v) = %d, want %d", c.name, c.tags, got, c.want)
		}
	}
}

func TestParseRecoverFailCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		want int
	}{
		{"present", []string{"umbrella", RecoverFailTag(2)}, 2},
		{"absent defaults", []string{"umbrella"}, 0},
		{"malformed defaults", []string{"umbrella-recover-fail:abc"}, 0},
		{"negative defaults", []string{"umbrella-recover-fail:-1"}, 0},
		{"nil defaults", nil, 0},
		{"duplicates take the max", []string{RecoverFailTag(1), RecoverFailTag(3), RecoverFailTag(2)}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseRecoverFailCount(c.tags); got != c.want {
				t.Errorf("ParseRecoverFailCount(%v) = %d, want %d", c.tags, got, c.want)
			}
		})
	}
}

func TestRecoverDue(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"absent is due", nil, true},
		{"malformed is due", []string{"umbrella-recover-after:not-a-number"}, true},
		{"future is not due", []string{RecoverAfterTag(now.Add(time.Hour))}, false},
		{"past is due", []string{RecoverAfterTag(now.Add(-time.Hour))}, true},
		{"exact instant is due", []string{RecoverAfterTag(now)}, true},
		{"duplicates take the latest", []string{RecoverAfterTag(now.Add(-time.Hour)), RecoverAfterTag(now.Add(time.Hour))}, false},
		{"malformed duplicate ignored alongside a valid future tag", []string{"umbrella-recover-after:not-a-number", RecoverAfterTag(now.Add(time.Hour))}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := RecoverDue(c.tags, now); got != c.want {
				t.Errorf("RecoverDue(%v, now) = %v, want %v", c.tags, got, c.want)
			}
		})
	}
}

func TestHasRecoverExhaustedTag(t *testing.T) {
	t.Parallel()
	if HasRecoverExhaustedTag([]string{"umbrella"}) {
		t.Fatal("HasRecoverExhaustedTag = true without the tag")
	}
	if !HasRecoverExhaustedTag([]string{"umbrella", RecoverExhaustedTag}) {
		t.Fatal("HasRecoverExhaustedTag = false with the tag present")
	}
}

func TestRecoverBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		failCount int
		want      time.Duration
	}{
		{0, time.Hour}, // treated as 1
		{1, time.Hour},
		{2, 2 * time.Hour},
		{3, 4 * time.Hour},
		{5, 16 * time.Hour},
		{6, 24 * time.Hour}, // 32h would exceed the cap
		{100, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := RecoverBackoff(c.failCount); got != c.want {
			t.Errorf("RecoverBackoff(%d) = %v, want %v", c.failCount, got, c.want)
		}
	}
}

func TestReplaceTagPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tags   []string
		prefix string
		newTag string
		want   []string
	}{
		{"replaces single match", []string{"umbrella", "umbrella-max-parallel:3"}, MaxParallelTagPrefix, "umbrella-max-parallel:5", []string{"umbrella", "umbrella-max-parallel:5"}},
		{"collapses duplicates", []string{"umbrella-max-parallel:3", "umbrella-max-parallel:9", "keep"}, MaxParallelTagPrefix, "umbrella-max-parallel:5", []string{"keep", "umbrella-max-parallel:5"}},
		{"appends when absent", []string{"umbrella"}, MaxParallelTagPrefix, "umbrella-max-parallel:5", []string{"umbrella", "umbrella-max-parallel:5"}},
		{"empty newTag just deletes", []string{"umbrella", "umbrella-max-parallel:3"}, MaxParallelTagPrefix, "", []string{"umbrella"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ReplaceTagPrefix(c.tags, c.prefix, c.newTag); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ReplaceTagPrefix(%v, %q, %q) = %v, want %v", c.tags, c.prefix, c.newTag, got, c.want)
			}
		})
	}
}

// TestRecoveryTagsAreNotInheritable guards against a recovery control tag
// leaking onto a child task's inherited GitHub labels — recovery tags carry
// the "umbrella-" prefix InheritableLabels already filters, so this test
// pins that invariant rather than re-deriving it.
func TestRecoveryTagsAreNotInheritable(t *testing.T) {
	t.Parallel()
	in := []string{RecoverFailTag(2), RecoverAfterTag(time.Unix(1_700_000_000, 0)), RecoverExhaustedTag, "keep"}
	got := InheritableLabels(in)
	if !reflect.DeepEqual(got, []string{"keep"}) {
		t.Errorf("InheritableLabels(%v) = %v, want [keep]", in, got)
	}
}
