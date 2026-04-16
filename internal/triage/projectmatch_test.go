package triage

import (
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

func TestMatchProject(t *testing.T) {
	projects := []project.Project{
		{ID: "Automaat/sybra", Owner: "Automaat", Repo: "sybra"},
		{ID: "foo/bar", Owner: "foo", Repo: "bar"},
	}
	tests := []struct {
		name, title, body, want string
	}{
		{"no url", "fix bug", "some description", ""},
		{"pr url in body", "review pr", "https://github.com/Automaat/sybra/pull/417", "Automaat/sybra"},
		{"issue url in title", "https://github.com/foo/bar/issues/1", "", "foo/bar"},
		{"url not registered", "check this", "https://github.com/unknown/repo", ""},
		{"url with .git suffix", "clone", "https://github.com/foo/bar.git", "foo/bar"},
		{"http (not https)", "legacy", "http://github.com/foo/bar/pull/9", "foo/bar"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchProject(tc.title, tc.body, projects)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
