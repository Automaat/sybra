package agentd

import (
	"testing"
	"time"
)

func TestAgentdOversubscriptionFactorResolveWith(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		host  int64
		want  int64
		hostN int
	}{
		{name: "an explicit override wins", env: "3", host: 8, want: 3},
		{name: "whitespace is tolerated", env: "  2 ", host: 8, want: 2},
		{name: "an empty value falls back to the host", env: "", host: 4, want: 4, hostN: 1},
		{name: "a non-numeric value falls back to the host", env: "lots", host: 4, want: 4, hostN: 1},
		{name: "a zero override falls back to the host", env: "0", host: 4, want: 4, hostN: 1},
		{name: "a negative override falls back to the host", env: "-2", host: 4, want: 4, hostN: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := agentdOversubscriptionFactorResolveWith(tc.env, func() int64 { calls++; return tc.host })
			if got != tc.want {
				t.Fatalf("factor = %d, want %d", got, tc.want)
			}
			if calls != tc.hostN {
				t.Fatalf("host factor consulted %d time(s), want %d", calls, tc.hostN)
			}
		})
	}
}

// A deadline may only ever grow: a misread load figure must not shorten the
// budget a test gives a run that is legitimately slow.
func TestScaledDeadlineNeverShrinks(t *testing.T) {
	base := 5 * time.Second
	if got := scaledDeadline(base); got < base {
		t.Fatalf("scaledDeadline(%s) = %s, want at least the base", base, got)
	}
}
