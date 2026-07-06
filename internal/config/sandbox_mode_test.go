package config

import "testing"

func TestNormalizeSandboxMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "report", false},
		{"report", "report", false},
		{"off", "off", false},
		{"enforce", "enforce", false},
		{"disabled", "", true},
		{"REPORT", "report", false},
		{"OFF", "off", false},
		{"Enforce", "enforce", false},
		{"enforce ", "enforce", false},
		{"  off  ", "off", false},
		{"   ", "report", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeSandboxMode(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultSandboxMode_FreshInstallResolvesReport pins the acceptance
// criterion that a fresh install with no explicit agent.sandbox_mode
// resolves to "report" (log-only, never blocks) rather than "off" or
// "enforce".
func TestDefaultSandboxMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, "report"},
		{"fresh install, empty field", &Config{}, "report"},
		{"valid off", &Config{Agent: AgentDefaults{SandboxMode: "off"}}, "off"},
		{"valid report", &Config{Agent: AgentDefaults{SandboxMode: "report"}}, "report"},
		{"valid enforce", &Config{Agent: AgentDefaults{SandboxMode: "enforce"}}, "enforce"},
		{"invalid → report fallback", &Config{Agent: AgentDefaults{SandboxMode: "disabled"}}, "report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.DefaultSandboxMode(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
