package sybra

import (
	"log/slog"
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
