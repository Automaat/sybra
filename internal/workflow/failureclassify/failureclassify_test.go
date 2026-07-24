package failureclassify

import "testing"

func TestIsGoInfraFailure(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"linker terminated", "# example\nlink: signal: terminated\n", true},
		{"go-build cache vanished", "can't open import: \"fmt\" in go-build1234/b001/importcfg", true},
		{"go-build no such file", `open /root/.cache/go-build/ab/cdef012: no such file or directory`, true},
		{"genuine compile error", "./foo.go:10:2: undefined: bar", false},
		{"empty output", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGoInfraFailure(tt.output); got != tt.want {
				t.Errorf("IsGoInfraFailure(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestIsMissingToolchain(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"command not found", "bash: golangci-lint: command not found", true},
		{"executable file not found", `exec: "npm": executable file not found in $PATH`, true},
		{"not found in path", "mise: tool not found in PATH", true},
		{"generic not found", "go.mod: not found", true},
		{"unrelated failure", "assertion failed: expected 1, got 2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMissingToolchain(tt.output); got != tt.want {
				t.Errorf("IsMissingToolchain(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	if got := InfraFailure.String(); got != "infra_failure" {
		t.Errorf("InfraFailure.String() = %q, want %q", got, "infra_failure")
	}
}
