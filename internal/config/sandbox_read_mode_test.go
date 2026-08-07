package config

import "testing"

func TestNormalizeSandboxReadMode(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to off", in: "", want: "off"},
		{name: "off", in: "off", want: "off"},
		{name: "report", in: "report", want: "report"},
		{name: "enforce", in: "enforce", want: "enforce"},
		{name: "case and space insensitive", in: "  ENFORCE ", want: "enforce"},
		{name: "unknown rejected", in: "strict", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSandboxReadMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeSandboxReadMode(%q) = %q, want error", tc.in, got)
					panic("unreachable")
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSandboxReadMode(%q): %v", tc.in, err)
				panic("unreachable")
			}
			if got != tc.want {
				t.Fatalf("NormalizeSandboxReadMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// An unset read posture must stay "off" rather than following sandbox_mode's
// "report" default, or upgrading would opt every deployment into resolving —
// and eventually enforcing — an allowlist it never asked for.
func TestNormalizeSandboxReadMode_EmptyDiffersFromSandboxMode(t *testing.T) {
	read, err := NormalizeSandboxReadMode("")
	if err != nil {
		t.Fatalf("NormalizeSandboxReadMode: %v", err)
		panic("unreachable")
	}
	write, err := NormalizeSandboxMode("")
	if err != nil {
		t.Fatalf("NormalizeSandboxMode: %v", err)
		panic("unreachable")
	}
	if read != "off" {
		t.Fatalf("empty sandbox_read_mode = %q, want off", read)
	}
	if read == write {
		t.Fatalf("sandbox_read_mode and sandbox_mode share the default %q; reads must stay opt-in", read)
	}
}

func TestDefaultSandboxReadMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{name: "nil config", cfg: nil, want: "off"},
		{name: "unset", cfg: &Config{}, want: "off"},
		{name: "enforce", cfg: configWithReadMode("enforce"), want: "enforce"},
		{name: "report", cfg: configWithReadMode("report"), want: "report"},
		// A typo must not fail every agent run closed on a missing read path.
		{name: "invalid degrades to off", cfg: configWithReadMode("bogus"), want: "off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.DefaultSandboxReadMode(); got != tc.want {
				t.Fatalf("DefaultSandboxReadMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func configWithReadMode(mode string) *Config {
	c := &Config{}
	c.Agent.SandboxReadMode = mode
	return c
}
