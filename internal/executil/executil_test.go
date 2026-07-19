package executil

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestEscapeAppleScript(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"clean path", "/home/user/project", "/home/user/project"},
		{"double quote", `say "hello"`, `say \"hello\"`},
		{"backslash", `foo\bar`, `foo\\bar`},
		{"backslash then quote", `foo\"bar`, `foo\\\"bar`},
		{"shell metachar semicolon", "foo; rm -rf /", "foo; rm -rf /"},
		{"newline", "foo\nbar", "foo\nbar"},
		{"single quote passthrough", "it's fine", "it's fine"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapeAppleScript(tt.input)
			if got != tt.want {
				t.Errorf("EscapeAppleScript(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRunEnv_NilEnvInheritsProcess(t *testing.T) {
	t.Setenv("EXECUTIL_TEST_MARKER", "inherited")
	if err := RunEnv(context.Background(), "", nil, "sh", "-c", `test "$EXECUTIL_TEST_MARKER" = "inherited"`); err != nil {
		t.Fatalf("RunEnv with nil env should inherit the process environment: %v", err)
	}
}

func TestRunEnv_ExplicitEnvOverridesProcess(t *testing.T) {
	t.Setenv("EXECUTIL_TEST_MARKER", "ambient")
	env := append(os.Environ(), "EXECUTIL_TEST_MARKER=overridden")
	if err := RunEnv(context.Background(), "", env, "sh", "-c", `test "$EXECUTIL_TEST_MARKER" = "overridden"`); err != nil {
		t.Fatalf("RunEnv should use the explicit env: %v", err)
	}
}

func TestRunEnv_ReturnsStderrOnFailure(t *testing.T) {
	err := RunEnv(context.Background(), "", nil, "sh", "-c", "echo boom 1>&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want it to contain stderr output", err)
	}
}

func TestRun_DelegatesToRunEnvWithNilEnv(t *testing.T) {
	t.Setenv("EXECUTIL_TEST_MARKER", "inherited")
	if err := Run(context.Background(), "", "sh", "-c", `test "$EXECUTIL_TEST_MARKER" = "inherited"`); err != nil {
		t.Fatalf("Run should behave like RunEnv with a nil env: %v", err)
	}
}
