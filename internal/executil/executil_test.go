package executil

import "testing"

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
