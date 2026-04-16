package poll

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
)

func TestRenovateHandlerRepos_FilterByProjectType(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	clones := t.TempDir()
	store, err := project.NewStore(dir, clones)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	writeProject(t, dir, "owner1--pet1.yaml", "owner1/pet1", "owner1", "pet1", project.ProjectTypePet)
	writeProject(t, dir, "owner1--pet2.yaml", "owner1/pet2", "owner1", "pet2", project.ProjectTypePet)
	writeProject(t, dir, "owner2--work1.yaml", "owner2/work1", "owner2", "work1", project.ProjectTypeWork)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name      string
		allowed   func(project.ProjectType) bool
		wantRepos []string
	}{
		{
			name:      "nil closure allows all",
			allowed:   nil,
			wantRepos: []string{"owner1/pet1", "owner1/pet2", "owner2/work1"},
		},
		{
			name:      "pet only",
			allowed:   func(t project.ProjectType) bool { return t == project.ProjectTypePet },
			wantRepos: []string{"owner1/pet1", "owner1/pet2"},
		},
		{
			name:      "work only",
			allowed:   func(t project.ProjectType) bool { return t == project.ProjectTypeWork },
			wantRepos: []string{"owner2/work1"},
		},
		{
			name:      "none",
			allowed:   func(project.ProjectType) bool { return false },
			wantRepos: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewRenovateHandler(store, logger, func(string, any) {}, &config.RenovateConfig{}, tt.allowed)
			got := h.Repos()
			assertReposEqual(t, got, tt.wantRepos)
		})
	}
}

func writeProject(t *testing.T, dir, filename, id, owner, repo string, ptype project.ProjectType) {
	t.Helper()
	body := "id: " + id + "\nname: " + repo + "\nowner: " + owner + "\nrepo: " + repo + "\ntype: " + string(ptype) + "\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}
}

func assertReposEqual(t *testing.T, got, want []string) {
	t.Helper()
	gotSet := make(map[string]bool, len(got))
	for _, r := range got {
		gotSet[r] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, r := range want {
		wantSet[r] = true
	}
	if len(gotSet) != len(wantSet) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	for r := range wantSet {
		if !gotSet[r] {
			t.Fatalf("missing repo %q in %v", r, got)
		}
	}
}
