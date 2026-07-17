package agent

import (
	"strings"
	"testing"
)

func TestNormalizeBashActionLabel_StripsEnvAssignmentsAndSecrets(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "leading env assignment stripped from generic bash label",
			command: `TOKEN=super-secret-value some-tool --flag`,
			want:    "bash:some-tool",
		},
		{
			name:    "inline auth header not embedded in generic bash label",
			command: `curl -H "Authorization: Bearer abc123" https://example.com`,
			want:    "bash:curl",
		},
		{
			name:    "check command keeps a short bounded prefix, drops secrets",
			command: `CI_TOKEN=abc123 go test ./... -run TestSecretLeak -v -count=1`,
			want:    "check:go test ./... -run",
		},
		{
			name:    "quoted env assignment with spaces is fully stripped",
			command: `TOKEN="super secret value" go test ./... -run TestSecretLeak -count=1`,
			want:    "check:go test ./... -run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBashActionLabel(tc.command)
			if got != tc.want {
				t.Fatalf("normalizeBashActionLabel(%q) = %q, want %q", tc.command, got, tc.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "abc123") {
				t.Fatalf("label leaked secret content: %q", got)
			}
		})
	}
}

func TestTruncateCommandFamily(t *testing.T) {
	cases := []struct {
		command    string
		keepTokens int
		want       string
	}{
		{"go test ./... -run Foo", 1, "go"},
		{"go test ./... -run Foo", 4, "go test ./... -run"},
		{"FOO=bar BAZ=qux npm ci", 1, "npm"},
		{`golangci-lint --token=abc123 run ./...`, 4, "golangci-lint --token=[redacted] run ./..."},
		{`golangci-lint --token abc123 run ./...`, 4, "golangci-lint --token [redacted] run"},
		{`TOKEN="super secret" go test ./... -run Foo`, 4, "go test ./... -run"},
		{"", 1, ""},
		{"FOO=bar", 1, ""},
	}
	for _, tc := range cases {
		if got := truncateCommandFamily(tc.command, tc.keepTokens); got != tc.want {
			t.Errorf("truncateCommandFamily(%q, %d) = %q, want %q", tc.command, tc.keepTokens, got, tc.want)
		}
	}
}

// TestNormalizeBashActionLabel_UnwrapsCodexShellDashC covers the false-positive
// semantic-loop kills seen live on codex agents (#2217-adjacent): codex's exec
// tool always shells out as `<shell> -c/-lc "<cmd>"`, and without unwrapping,
// every such call's outer wrapper — not the real inner command — was what got
// classified, collapsing distinct reads/greps/tests into one identical
// "bash:/bin/bash" family regardless of what they actually did.
func TestNormalizeBashActionLabel_UnwrapsCodexShellDashC(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "bash -lc read of file A",
			command: `/bin/bash -lc "sed -n '940,1085p' internal/sybra/app_human_review.go"`,
			want:    "read:internal/sybra/app_human_review.go",
		},
		{
			name:    "bash -lc read of file B — must differ from file A",
			command: `/bin/bash -lc "sed -n '1,220p' internal/sybra/completion/completion.go"`,
			want:    "read:internal/sybra/completion/completion.go",
		},
		{
			name:    "bash -lc long-running check",
			command: `/bin/bash -lc "go test ./internal/sybra -run TestFoo"`,
			want:    "check:go test ./internal/sybra -run",
		},
		{
			name:    "sh -c generic command",
			command: `sh -c "git status --short"`,
			want:    "bash:git",
		},
		{
			name:    "no shell wrapper stays unaffected",
			command: `sed -n '1,10p' foo.go`,
			want:    "read:foo.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeBashActionLabel(tc.command); got != tc.want {
				t.Fatalf("normalizeBashActionLabel(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func TestNormalizeBashActionLabel_HeadTailWithoutFileStayGeneric(t *testing.T) {
	for _, cmd := range []string{"tail -n 20", "head -n 20", "tail --lines 20", "head --bytes 20"} {
		want := "bash:" + strings.Fields(cmd)[0]
		if got := normalizeBashActionLabel(cmd); got != want {
			t.Fatalf("normalizeBashActionLabel(%q) = %q, want %q", cmd, got, want)
		}
	}
}
