package sybra

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
)

// TestInitIssuesFetcher_GitHubDisabled_NoFetcherRegistered verifies the
// machine-level kill switch: flipping GitHub.Enabled off means the Issues
// fetcher is never constructed, so startPollHub has nothing to register.
func TestInitIssuesFetcher_GitHubDisabled_NoFetcherRegistered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		enabled bool
		wantNil bool
	}{
		{"github disabled returns nil", false, true},
		{"github enabled returns fetcher", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := setupApp(t)
			a.cfg = &config.Config{GitHub: config.GitHubConfig{Enabled: tt.enabled}}

			got := a.initIssuesFetcher(func(string, any) {})

			if tt.wantNil && got != nil {
				t.Fatalf("initIssuesFetcher = %v, want nil when GitHub.Enabled=false", got)
			}
			if !tt.wantNil && got == nil {
				t.Fatal("initIssuesFetcher = nil, want non-nil when GitHub.Enabled=true")
			}
		})
	}
}

// TestAllowsProjectType_RoutingAcrossMachines verifies the config-driven
// routing closure that IssuesFetcher/RenovateHandler receive. Two configs
// (pet-only vs work-only) should answer different sets of project types.
func TestAllowsProjectType_RoutingAcrossMachines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       *config.Config
		wantPet   bool
		wantWork  bool
		wantOther bool
	}{
		{
			name:      "pet-only machine",
			cfg:       &config.Config{ProjectTypes: []string{"pet"}},
			wantPet:   true,
			wantWork:  false,
			wantOther: false,
		},
		{
			name:      "work-only machine",
			cfg:       &config.Config{ProjectTypes: []string{"work"}},
			wantPet:   false,
			wantWork:  true,
			wantOther: false,
		},
		{
			name:      "unrestricted machine",
			cfg:       &config.Config{},
			wantPet:   true,
			wantWork:  true,
			wantOther: true,
		},
		{
			name:      "explicit both",
			cfg:       &config.Config{ProjectTypes: []string{"pet", "work"}},
			wantPet:   true,
			wantWork:  true,
			wantOther: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &App{cfg: tt.cfg, logger: slog.Default()}

			if got := a.allowsProjectType(project.ProjectTypePet); got != tt.wantPet {
				t.Errorf("allowsProjectType(pet) = %v, want %v", got, tt.wantPet)
			}
			if got := a.allowsProjectType(project.ProjectTypeWork); got != tt.wantWork {
				t.Errorf("allowsProjectType(work) = %v, want %v", got, tt.wantWork)
			}
			if got := a.allowsProjectType(project.ProjectType("other")); got != tt.wantOther {
				t.Errorf("allowsProjectType(other) = %v, want %v", got, tt.wantOther)
			}
		})
	}
}

// TestCanFilePublicForProject covers the Work-Data Confidentiality guard:
// auto-filing on the public sybra repo must be blocked for work-typed
// projects regardless of per-machine routing config. Pet/unknown projects
// and tasks without a project_id remain allowed.
func TestCanFilePublicForProject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Seed projects by writing YAML directly — Create() does a real git clone.
	mustWriteProjectYAML(t, dir, "work-owner/work-repo", project.ProjectTypeWork)
	mustWriteProjectYAML(t, dir, "pet-owner/pet-repo", project.ProjectTypePet)

	// Even a machine routed to handle BOTH project types must still block
	// public-repo filings for work-typed tasks.
	a := &App{
		cfg:      &config.Config{ProjectTypes: []string{"pet", "work"}},
		projects: store,
		logger:   slog.Default(),
	}

	cases := []struct {
		name      string
		projectID string
		want      bool
	}{
		{"empty projectID is allowed", "", true},
		{"pet project allowed", "pet-owner/pet-repo", true},
		{"work project blocked", "work-owner/work-repo", false},
		{"unknown project fails open", "ghost-owner/ghost-repo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := a.canFilePublicForProject(tc.projectID); got != tc.want {
				t.Errorf("canFilePublicForProject(%q) = %v, want %v", tc.projectID, got, tc.want)
			}
		})
	}
}

// mustWriteProjectYAML writes a minimal project record straight to the
// project.Store directory. Bypasses Store.Create (which performs a real git
// clone) so the test can stage work/pet entries without network access.
func mustWriteProjectYAML(t *testing.T, dir, id string, ptype project.ProjectType) {
	t.Helper()
	// project.Store.filePath maps "owner/repo" → "owner--repo.yaml".
	safe := id
	for i := 0; i < len(safe); i++ {
		if safe[i] == '/' {
			safe = safe[:i] + "--" + safe[i+1:]
		}
	}
	path := filepath.Join(dir, safe+".yaml")
	content := "id: " + id + "\ntype: " + string(ptype) + "\nowner: stub\nrepo: stub\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write project YAML: %v", err)
	}
}
