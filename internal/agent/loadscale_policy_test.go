package agent

import "testing"

func TestHostOversubscriptionFactorResolveWithEnvOverride(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		host     int64
		want     int64
		wantHost bool
	}{
		{name: "explicit positive override wins", envValue: "1", host: 7, want: 1},
		{name: "override is trimmed", envValue: " 3 ", host: 7, want: 3},
		{name: "invalid override falls back", envValue: "nope", host: 4, want: 4, wantHost: true},
		{name: "zero override falls back", envValue: "0", host: 5, want: 5, wantHost: true},
		{name: "empty override uses host", envValue: "", host: 6, want: 6, wantHost: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			got := hostOversubscriptionFactorResolveWith(tt.envValue, func() int64 {
				called = true
				return tt.host
			})
			if got != tt.want {
				t.Fatalf("hostOversubscriptionFactorResolveWith(%q) = %d, want %d", tt.envValue, got, tt.want)
			}
			if called != tt.wantHost {
				t.Fatalf("host factor called = %v, want %v", called, tt.wantHost)
			}
		})
	}
}
