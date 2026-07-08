package sybra

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/review"
)

// TestInitIssuesFetcher_GitHubDisabled_NoFetcherRegistered verifies the
// machine-level kill switch and the issues-specific sub-toggle: either one
// being off means the Issues fetcher is never constructed, so startPollHub
// has nothing to register.
func TestInitIssuesFetcher_GitHubDisabled_NoFetcherRegistered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		enabled       bool
		issuesEnabled bool
		wantNil       bool
	}{
		{"github disabled returns nil", false, true, true},
		{"github enabled, issues enabled returns fetcher", true, true, false},
		{"github enabled, issues_enabled false returns nil", true, false, true},
		{"github disabled, issues_enabled true still returns nil", false, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := setupApp(t)
			a.cfg = &config.Config{GitHub: config.GitHubConfig{Enabled: tt.enabled, IssuesEnabled: tt.issuesEnabled}}

			got := a.initIssuesFetcher(func(string, any) {})

			if tt.wantNil && got != nil {
				t.Fatalf("initIssuesFetcher = %v, want nil", got)
			}
			if !tt.wantNil && got == nil {
				t.Fatal("initIssuesFetcher = nil, want non-nil")
			}
		})
	}
}

// fakePollRegistrar is a pollRegistrar test double that records the Name() of
// every registered Fetcher, so tests can assert on registration decisions
// without reaching into poll.Hub's private fields.
type fakePollRegistrar struct {
	registered []string
}

func (f *fakePollRegistrar) Register(fetcher poll.Fetcher, _ time.Duration) {
	f.registered = append(f.registered, fetcher.Name())
}

// TestPollHubReviewerRegistration_GitHubReviewToggles verifies the fix at the
// heart of this task: the PR reviewer poll used to register unconditionally
// whenever a.reviewer was non-nil, regardless of github.enabled. It must now
// register only when the effective RunsReviewer() state is true.
func TestPollHubReviewerRegistration_GitHubReviewToggles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		enabled        bool
		reviewsEnabled bool
		wantRegistered bool
	}{
		{"github enabled, reviews enabled registers reviewer", true, true, true},
		{"github enabled, reviews_enabled false skips reviewer", true, false, false},
		{"github disabled skips reviewer even if reviews_enabled true", false, true, false},
		{"github disabled and reviews_enabled false skips reviewer", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &App{
				cfg:      &config.Config{GitHub: config.GitHubConfig{Enabled: tt.enabled, ReviewsEnabled: tt.reviewsEnabled}},
				logger:   discardLogger(),
				reviewer: review.New(nil, nil, nil, nil, discardLogger(), nil, nil, nil, nil, nil, nil),
			}

			reg := &fakePollRegistrar{}
			registerPollHandlers(a, reg, nil)

			gotRegistered := slices.Contains(reg.registered, "reviews")
			if gotRegistered != tt.wantRegistered {
				t.Errorf("reviewer registered = %v, want %v (registered: %v)", gotRegistered, tt.wantRegistered, reg.registered)
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

// TestWorkScrubContextForTask covers the Work-Data Confidentiality guard:
// work-typed projects must yield a non-nil scrub context (signalling the
// caller to scrub + route to a local task) regardless of per-machine
// routing config. Pet/unknown projects and tasks without a project_id
// must yield nil so artifacts file normally on the public repo.
func TestWorkScrubContextForTask(t *testing.T) {
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
		name        string
		projectID   string
		wantNonNil  bool
		wantInBlock []string // substrings the blocklist must contain when non-nil
	}{
		{name: "empty projectID is unscoped", projectID: "", wantNonNil: false},
		{name: "pet project is unscoped", projectID: "pet-owner/pet-repo", wantNonNil: false},
		{name: "unknown project fails open", projectID: "ghost-owner/ghost-repo", wantNonNil: false},
		{
			name:        "work project produces blocklist",
			projectID:   "work-owner/work-repo",
			wantNonNil:  true,
			wantInBlock: []string{"work-owner/work-repo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := a.workScrubContextForTask(tc.projectID)
			if (got != nil) != tc.wantNonNil {
				t.Fatalf("workScrubContextForTask(%q) non-nil=%v, want %v", tc.projectID, got != nil, tc.wantNonNil)
			}
			if got == nil {
				return
			}
			for _, needle := range tc.wantInBlock {
				if !slices.Contains(got.Blocklist, needle) {
					t.Errorf("blocklist missing %q; got %v", needle, got.Blocklist)
				}
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
