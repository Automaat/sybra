package sybra

import (
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

func TestIsWorkProjectFailsSafe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustWriteProjectYAML(t, dir, "owner/work", project.ProjectTypeWork)
	mustWriteProjectYAML(t, dir, "owner/pet", project.ProjectTypePet)

	a := &App{projects: store, logger: slog.Default()}

	cases := []struct {
		name      string
		projectID string
		want      bool
	}{
		{"work project is work", "owner/work", true},
		{"pet project is not work", "owner/pet", false},
		{"unregistered project fails safe to work", "owner/ghost", true},
		{"empty projectID is not work", "", false},
	}
	for _, c := range cases {
		if got := a.isWorkProject(c.projectID); got != c.want {
			t.Errorf("%s: isWorkProject(%q) = %v, want %v", c.name, c.projectID, got, c.want)
		}
	}

	nilProjects := &App{logger: slog.Default()}
	if !nilProjects.isWorkProject("owner/x") {
		t.Error("nil project store must fail safe (treat as work)")
	}
}
