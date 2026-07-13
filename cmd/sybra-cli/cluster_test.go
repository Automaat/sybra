package main

import "testing"

func TestSplitLeadingID(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantID   string
		wantRest []string
	}{
		{
			name:     "documented id-first order",
			args:     []string{"task-1", "--node", "pet-box"},
			wantID:   "task-1",
			wantRest: []string{"--node", "pet-box"},
		},
		{
			name:     "flags-first order still works",
			args:     []string{"--node", "pet-box", "task-1"},
			wantID:   "",
			wantRest: []string{"--node", "pet-box", "task-1"},
		},
		{
			name:     "no args",
			args:     nil,
			wantID:   "",
			wantRest: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, rest := splitLeadingID(tc.args)
			if id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
			if len(rest) != len(tc.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
			for i := range rest {
				if rest[i] != tc.wantRest[i] {
					t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
				}
			}
		})
	}
}
