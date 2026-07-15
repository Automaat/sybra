package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
)

func TestResolveHeadlessPermissionMode(t *testing.T) {
	t.Parallel()

	cfgAuto := &config.Config{Agent: config.AgentDefaults{HeadlessPermissionMode: "auto"}}
	cfgBypass := &config.Config{Agent: config.AgentDefaults{HeadlessPermissionMode: "bypass"}}
	cfgEmpty := &config.Config{}

	cases := []struct {
		name    string
		t       task.Task
		cfg     *config.Config
		want    string
		wantErr bool
	}{
		{
			name: "task auto overrides config bypass",
			t:    task.Task{ID: "t1", HeadlessPermissionMode: "auto"},
			cfg:  cfgBypass,
			want: "auto",
		},
		{
			name: "task bypass overrides config auto",
			t:    task.Task{ID: "t2", HeadlessPermissionMode: "bypass"},
			cfg:  cfgAuto,
			want: "bypass",
		},
		{
			name: "task empty falls back to config auto",
			t:    task.Task{ID: "t3"},
			cfg:  cfgAuto,
			want: "auto",
		},
		{
			name: "task empty, config empty → bypass default",
			t:    task.Task{ID: "t4"},
			cfg:  cfgEmpty,
			want: "bypass",
		},
		{
			name: "task empty, nil config → bypass default",
			t:    task.Task{ID: "t5"},
			cfg:  nil,
			want: "bypass",
		},
		{
			name:    "invalid task value → error (abort, no fallback)",
			t:       task.Task{ID: "t6", HeadlessPermissionMode: "dangerously-skip"},
			cfg:     cfgBypass,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := agentorch.ResolveHeadlessPermissionMode(tc.t, tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
