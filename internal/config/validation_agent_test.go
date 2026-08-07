package config

import "testing"

func TestValidateResolvedConfig_ClassReservations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantMsg string
		wantOK  bool
	}{
		{
			name:   "unset is valid (zero-regression default)",
			mutate: func(c *Config) {},
			wantOK: true,
		},
		{
			name: "known classes within budget are valid",
			mutate: func(c *Config) {
				c.Agent.MaxConcurrent = 10
				c.Agent.ClassReservations = map[string]int{"implementation": 2, "completion": 1, "system": 1}
			},
			wantOK: true,
		},
		{
			name: "unknown class key rejected",
			mutate: func(c *Config) {
				c.Agent.ClassReservations = map[string]int{"recovery": 1}
			},
			wantMsg: `agent.class_reservations: unknown class "recovery"`,
		},
		{
			name: "negative reserved minimum rejected",
			mutate: func(c *Config) {
				c.Agent.ClassReservations = map[string]int{"system": -1}
			},
			wantMsg: "agent.class_reservations.system: reserved minimum must be >= 0",
		},
		{
			name: "sum exceeding max_concurrent rejected",
			mutate: func(c *Config) {
				c.Agent.MaxConcurrent = 2
				c.Agent.ClassReservations = map[string]int{"implementation": 1, "completion": 1, "system": 1}
			},
			wantMsg: "agent.class_reservations: sum of reserved minimums (3) exceeds agent.max_concurrent (2)",
		},
		{
			name: "sum equal to max_concurrent is valid",
			mutate: func(c *Config) {
				c.Agent.MaxConcurrent = 3
				c.Agent.ClassReservations = map[string]int{"implementation": 1, "completion": 1, "system": 1}
			},
			wantOK: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			c.mutate(cfg)
			err := ValidateResolvedConfig(cfg)
			if c.wantOK {
				if err != nil {
					t.Fatalf("ValidateResolvedConfig() err = %v, want nil", err)
					panic("unreachable")
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateResolvedConfig() err = nil, want error containing %q", c.wantMsg)
				panic("unreachable")
			}
			if msgs := ValidationMessages(err); !containsMsg(msgs, c.wantMsg) {
				t.Fatalf("messages = %v, want one containing %q", msgs, c.wantMsg)
			}
		})
	}
}
