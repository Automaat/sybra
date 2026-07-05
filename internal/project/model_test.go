package project

import "testing"

func TestProject_IsSybraProject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		owner string
		repo  string
		want  bool
	}{
		{"exact match", "Automaat", "sybra", true},
		{"case-insensitive", "automaat", "SYBRA", true},
		{"different repo", "Automaat", "sybra-testbed", false},
		{"different owner", "someone-else", "sybra", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := Project{Owner: tt.owner, Repo: tt.repo}
			if got := p.IsSybraProject(); got != tt.want {
				t.Errorf("IsSybraProject() = %v, want %v", got, tt.want)
			}
		})
	}
}
