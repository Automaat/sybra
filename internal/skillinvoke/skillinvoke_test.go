package skillinvoke

import "testing"

func TestNormalizeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "sybra-test", want: "sybra-test", ok: true},
		{in: "/sybra-test", want: "sybra-test", ok: true},
		{in: "", ok: false},
		{in: " ", ok: false},
		{in: "Sybra-Test", ok: false},
		{in: "$sybra-test", ok: false},
		{in: "tmp/sybra-test", ok: false},
		{in: "sybra_test", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeName(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("NormalizeName(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRewriteInvocations(t *testing.T) {
	t.Parallel()
	got := RewriteInvocations("Run /plan-critic, not /tmp/plan-critic or /plan-critic-extra.", []string{"plan", "plan-critic"})
	want := "Run $plan-critic, not /tmp/plan-critic or /plan-critic-extra."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyAliases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prompt  string
		aliases map[string]string
		want    string
	}{
		{
			name:    "slash aliases",
			prompt:  "Run /sybra-test now.",
			aliases: map[string]string{"/sybra-test": "/sybra-test-v2"},
			want:    "Run /sybra-test-v2 now.",
		},
		{
			name:    "boundary and path",
			prompt:  "/sybra-test /tmp/sybra-test.md /sybra-test-extra x/sybra-test",
			aliases: map[string]string{"sybra-test": "sybra-test-v2"},
			want:    "/sybra-test-v2 /tmp/sybra-test.md /sybra-test-extra x/sybra-test",
		},
		{
			name:    "overlap longest wins",
			prompt:  "/plan-critic /plan",
			aliases: map[string]string{"plan": "plan-v2", "plan-critic": "plan-critic-v2"},
			want:    "/plan-critic-v2 /plan-v2",
		},
		{
			name:    "no chaining",
			prompt:  "/a",
			aliases: map[string]string{"a": "b", "b": "c"},
			want:    "/b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ApplyAliases(tt.prompt, tt.aliases); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
