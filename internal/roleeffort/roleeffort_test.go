package roleeffort

import "testing"

func TestForRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role string
		want string
	}{
		{"triage", "low"},
		{"eval", "low"},
		{"plan-critic", "low"},
		{"human-review", "low"},
		{"monitor", "low"},
		{"plan", "high"},
		{"review", "high"},
		{"fix-review", "high"},
		{"pr-fix", "high"},
		{"test-fix", "high"},
		{"implementation", ""},
		{"test-runner", ""},
		{"loop", ""},
		{"orchestrator", ""},
		{"", ""},
		{"not-a-role", ""},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			t.Parallel()
			if got := ForRole(tt.role); got != tt.want {
				t.Errorf("ForRole(%q) = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role string
		want string
	}{
		{"review", "high"},
		{"triage", "low"},
		{"implementation", Global},
		{"not-a-role", Global},
		{"", Global},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			t.Parallel()
			if got := Resolve(tt.role); got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}
