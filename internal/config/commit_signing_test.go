package config

import "testing"

func TestNormalizeCommitSigning(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"", "auto", false},
		{"auto", "auto", false},
		{"never", "never", false},
		{"require", "require", false},
		{"  NEVER  ", "never", false},
		{"off", "", true},
	}
	for _, tc := range tests {
		got, err := NormalizeCommitSigning(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeCommitSigning(%q) = %q, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeCommitSigning(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeCommitSigning(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// An invalid value must degrade to the historical host-probing behavior
// rather than silently refusing or forcing signatures.
func TestConfigCommitSigning_FallsBackToAuto(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.CommitSigning(); got != "auto" {
		t.Errorf("nil Config.CommitSigning() = %q, want auto", got)
	}
	cfg := &Config{}
	cfg.Agent.CommitSigning = "nonsense"
	if got := cfg.CommitSigning(); got != "auto" {
		t.Errorf("invalid Config.CommitSigning() = %q, want auto", got)
	}
	cfg.Agent.CommitSigning = "never"
	if got := cfg.CommitSigning(); got != "never" {
		t.Errorf("Config.CommitSigning() = %q, want never", got)
	}
}
