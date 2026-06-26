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
