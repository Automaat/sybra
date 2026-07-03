package sybra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

func newExperienceProjectStore(t *testing.T, tmp string) *project.Store {
	t.Helper()
	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	return projects
}

func seedExperienceProject(t *testing.T, dir string, proj project.Project) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if proj.Name == "" {
		proj.Name = proj.Repo
	}
	if proj.ClonePath == "" {
		proj.ClonePath = filepath.Join(t.TempDir(), proj.Repo+".git")
	}
	var b strings.Builder
	b.WriteString("id: " + proj.ID + "\n")
	b.WriteString("name: " + proj.Name + "\n")
	b.WriteString("owner: " + proj.Owner + "\n")
	b.WriteString("repo: " + proj.Repo + "\n")
	b.WriteString("url: " + proj.URL + "\n")
	b.WriteString("clone_path: " + proj.ClonePath + "\n")
	b.WriteString("type: " + string(proj.Type) + "\n")
	if proj.Checks != nil && len(proj.Checks.Verify) > 0 {
		b.WriteString("checks:\n  verify:\n")
		for _, cmd := range proj.Checks.Verify {
			b.WriteString("    - " + cmd + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(proj.ID, "/", "--")+".yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
