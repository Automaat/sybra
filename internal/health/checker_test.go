package health

import "testing"

func TestOwnedProcessesOwnsSeparatesPIDsAndGroups(t *testing.T) {
	t.Parallel()

	owned := OwnedProcesses{
		PIDs:          map[int]bool{100: true},
		ProcessGroups: map[int]bool{200: true},
	}

	tests := []struct {
		name string
		pid  int
		pgid int
		want bool
	}{
		{"exact pid", 100, 999, true},
		{"trusted process group", 101, 200, true},
		{"pid set is not a process group", 101, 100, false},
		{"unknown", 101, 999, false},
		{"zero values", 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := owned.Owns(tt.pid, tt.pgid); got != tt.want {
				t.Fatalf("Owns(%d, %d) = %v, want %v", tt.pid, tt.pgid, got, tt.want)
			}
		})
	}
}
