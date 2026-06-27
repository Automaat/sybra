package config

import (
	"testing"
)

func TestNormalizeHeadlessPermissionMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "bypass", false},
		{"bypass", "bypass", false},
		{"auto", "auto", false},
		{"dangerously-skip", "", true},
		{"BYPASS", "", true},
		{"AUTO", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeHeadlessPermissionMode(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultHeadlessPermissionMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, "bypass"},
		{"empty field", &Config{}, "bypass"},
		{"valid bypass", &Config{Agent: AgentDefaults{HeadlessPermissionMode: "bypass"}}, "bypass"},
		{"valid auto", &Config{Agent: AgentDefaults{HeadlessPermissionMode: "auto"}}, "auto"},
		{"invalid → bypass fallback", &Config{Agent: AgentDefaults{HeadlessPermissionMode: "invalid"}}, "bypass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.DefaultHeadlessPermissionMode(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
