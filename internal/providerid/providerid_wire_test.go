package providerid

import (
	"slices"
	"testing"
)

// The constants exist for Go comparisons, but their values are also the wire
// format: config keys, persisted task/agent records and CLI flags all carry
// these exact strings. Changing one would silently orphan every record and
// config file already on disk, so pin the literals here rather than letting a
// rename look harmless.
func TestConstantsMatchThePersistedWireStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		got  string
		want string
	}{
		{got: Claude, want: "claude"},
		{got: Codex, want: "codex"},
		{got: Copilot, want: "copilot"},
		{got: OpenCode, want: "opencode"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("constant = %q, want %q", tt.got, tt.want)
		}
		if !IsKnown(tt.want) {
			t.Errorf("IsKnown(%q) = false, want true", tt.want)
		}
	}
}

// All() drives failover order and is derived from the constants, so a
// reordering or a dropped entry would change dispatch behaviour silently.
func TestAllIsDerivedFromConstantsInFailoverOrder(t *testing.T) {
	t.Parallel()
	want := []string{Claude, Codex, Copilot, OpenCode}
	if got := All(); !slices.Equal(got, want) {
		t.Errorf("All() = %v, want %v", got, want)
	}
	if got, want := List(), "claude, codex, copilot, opencode"; got != want {
		t.Errorf("List() = %q, want %q", got, want)
	}
}
