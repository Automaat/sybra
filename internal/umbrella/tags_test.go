package umbrella

import (
	"reflect"
	"testing"
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
