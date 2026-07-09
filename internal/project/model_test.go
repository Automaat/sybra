package project

import (
	"slices"
	"testing"
)

func TestProject_WorkBlocklist(t *testing.T) {
	t.Parallel()
	work := Project{ID: "acme/api", Owner: "acme", Repo: "api", URL: "https://github.com/acme/api", Type: ProjectTypeWork}
	if got := work.WorkBlocklist(); !slices.Equal(got, []string{"acme/api", "acme", "api", "https://github.com/acme/api"}) {
		t.Fatalf("work blocklist = %v", got)
	}

	noURL := Project{ID: "acme/api", Owner: "acme", Repo: "api", Type: ProjectTypeWork}
	if got := noURL.WorkBlocklist(); !slices.Equal(got, []string{"acme/api", "acme", "api"}) {
		t.Fatalf("no-url blocklist = %v", got)
	}

	pet := Project{ID: "me/pet", Owner: "me", Repo: "pet", URL: "https://github.com/me/pet", Type: ProjectTypePet}
	if got := pet.WorkBlocklist(); got != nil {
		t.Fatalf("pet blocklist = %v, want nil", got)
	}
}

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
