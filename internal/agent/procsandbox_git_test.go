package agent

import (
	"slices"
	"testing"
)

func TestDedupeRoots_DropsEmptyAndDuplicate(t *testing.T) {
	got := dedupeRoots("/a", "/a", "", "/b", "  ", "/a")
	if !slices.Equal(got, []string{"/a", "/b"}) {
		t.Errorf("dedupeRoots = %v, want [/a /b]", got)
	}
}
