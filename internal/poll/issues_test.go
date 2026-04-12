package poll

import (
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
)

// issuesFetcherEnv holds the fully-wired dependencies for a single-machine
// test scenario. Real task.Manager and real project.Store are used; only
// the outbound gh calls are stubbed.
type issuesFetcherEnv struct {
	fetcher     *IssuesFetcher
	tasks       *task.Manager
	projects    *project.Store
	projectsDir string
}

// newIssuesFetcherForTest wires an IssuesFetcher with real Manager/Store + an
// injected labeled-issues fetcher so tests drive the full sync pipeline
// without touching the gh CLI.
func newIssuesFetcherForTest(
	t *testing.T,
	allowsType func(project.ProjectType) bool,
	labeled []github.Issue,
) *issuesFetcherEnv {
	t.Helper()

	projectsDir := t.TempDir()
	clonesDir := t.TempDir()
	projStore, err := project.NewStore(projectsDir, clonesDir)
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}

	taskStore, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := NewIssuesFetcher(taskMgr, projStore, func(string, any) {}, logger, allowsType)

	// Inject the labeled fetch so tests control the "gh" response.
	f.fetchLabeled = func([]string, string) ([]github.Issue, error) {
		return labeled, nil
	}
	// Assigned path is exercised separately; default to empty to avoid gh.
	f.fetchAssigned = func() ([]github.Issue, error) { return nil, nil }

	return &issuesFetcherEnv{
		fetcher:     f,
		tasks:       taskMgr,
		projects:    projStore,
		projectsDir: projectsDir,
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_FiltersByProjectType(t *testing.T) {
	t.Parallel()

	petOnly := func(pt project.ProjectType) bool { return pt == project.ProjectTypePet }
	workOnly := func(pt project.ProjectType) bool { return pt == project.ProjectTypeWork }
	allowAll := func(project.ProjectType) bool { return true }

	issues := []github.Issue{
		{Number: 1, Title: "pet1 issue", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
		{Number: 2, Title: "pet2 issue", URL: "https://github.com/acme/pet2/issues/2", Repository: "acme/pet2"},
		{Number: 3, Title: "work1 issue", URL: "https://github.com/bigco/work1/issues/3", Repository: "bigco/work1"},
		{Number: 4, Title: "unregistered", URL: "https://github.com/ext/tool/issues/4", Repository: "ext/tool"},
	}

	tests := []struct {
		name       string
		allowsType func(project.ProjectType) bool
		// wantIssues is the set of issue URLs expected to have resulted in a
		// task. Unregistered repos always pass through.
		wantIssues []string
	}{
		{
			name:       "pet-only machine skips work repos",
			allowsType: petOnly,
			wantIssues: []string{
				"https://github.com/acme/pet1/issues/1",
				"https://github.com/acme/pet2/issues/2",
				"https://github.com/ext/tool/issues/4",
			},
		},
		{
			name:       "work-only machine skips pet repos",
			allowsType: workOnly,
			wantIssues: []string{
				"https://github.com/bigco/work1/issues/3",
				"https://github.com/ext/tool/issues/4",
			},
		},
		{
			name:       "allow-all accepts every registered and unregistered repo",
			allowsType: allowAll,
			wantIssues: []string{
				"https://github.com/acme/pet1/issues/1",
				"https://github.com/acme/pet2/issues/2",
				"https://github.com/bigco/work1/issues/3",
				"https://github.com/ext/tool/issues/4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newIssuesFetcherForTest(t, tt.allowsType, nil)
			writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
			writeProject(t, env.projectsDir, "acme--pet2.yaml", "acme/pet2", "acme", "pet2", project.ProjectTypePet)
			writeProject(t, env.projectsDir, "bigco--work1.yaml", "bigco/work1", "bigco", "work1", project.ProjectTypeWork)

			env.fetcher.syncIssuesToTasks(issues)

			gotURLs := taskIssueURLs(t, env.tasks)
			assertStringSetEqual(t, gotURLs, tt.wantIssues)
		})
	}
}

func TestIssuesFetcher_SyncLabeledIssuesToTasks_HonorsClosure(t *testing.T) {
	t.Parallel()

	// Labeled fetch returns the full set; the fetcher is expected to narrow
	// the repos it asks about via allowsType, and to drop any labeled results
	// whose repo type isn't allowed even if the stub returned them.
	labeled := []github.Issue{
		{Number: 10, Title: "pet labeled", URL: "https://github.com/acme/pet1/issues/10", Repository: "acme/pet1"},
		{Number: 11, Title: "work labeled", URL: "https://github.com/bigco/work1/issues/11", Repository: "bigco/work1"},
	}

	tests := []struct {
		name       string
		allowsType func(project.ProjectType) bool
		// wantAskedRepos is the set of repos the labeled fetcher was asked
		// about (proves the closure narrows the query).
		wantAskedRepos []string
		// wantTaskURLs is the set of issue URLs that should have produced tasks.
		wantTaskURLs []string
	}{
		{
			name:           "pet-only asks for pet repos only",
			allowsType:     func(pt project.ProjectType) bool { return pt == project.ProjectTypePet },
			wantAskedRepos: []string{"acme/pet1"},
			wantTaskURLs:   []string{"https://github.com/acme/pet1/issues/10"},
		},
		{
			name:           "work-only asks for work repos only",
			allowsType:     func(pt project.ProjectType) bool { return pt == project.ProjectTypeWork },
			wantAskedRepos: []string{"bigco/work1"},
			wantTaskURLs:   []string{"https://github.com/bigco/work1/issues/11"},
		},
		{
			name:           "allow-all asks for all repos",
			allowsType:     func(project.ProjectType) bool { return true },
			wantAskedRepos: []string{"acme/pet1", "bigco/work1"},
			wantTaskURLs: []string{
				"https://github.com/acme/pet1/issues/10",
				"https://github.com/bigco/work1/issues/11",
			},
		},
		{
			name:           "deny-all never calls labeled fetcher",
			allowsType:     func(project.ProjectType) bool { return false },
			wantAskedRepos: nil,
			wantTaskURLs:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newIssuesFetcherForTest(t, tt.allowsType, labeled)
			writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
			writeProject(t, env.projectsDir, "bigco--work1.yaml", "bigco/work1", "bigco", "work1", project.ProjectTypeWork)

			var askedRepos []string
			asked := false
			env.fetcher.fetchLabeled = func(repos []string, label string) ([]github.Issue, error) {
				asked = true
				askedRepos = append([]string(nil), repos...)
				if label != "synapse" {
					t.Errorf("label = %q, want %q", label, synapseIssueLabel)
				}
				return labeled, nil
			}

			env.fetcher.syncLabeledIssuesToTasks()

			if len(tt.wantAskedRepos) == 0 && asked {
				t.Fatalf("fetchLabeled was called with %v, want no call", askedRepos)
			}
			if len(tt.wantAskedRepos) > 0 {
				assertStringSetEqual(t, askedRepos, tt.wantAskedRepos)
			}
			assertStringSetEqual(t, taskIssueURLs(t, env.tasks), tt.wantTaskURLs)
		})
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_SkipsAlreadyTracked(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	existing, err := env.tasks.Create("already there", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := env.tasks.Update(existing.ID, task.Update{
		Issue: task.Ptr("https://github.com/acme/pet1/issues/1"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	env.fetcher.syncIssuesToTasks([]github.Issue{
		{Number: 1, Title: "pet1 issue", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
	})

	tasks, err := env.tasks.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1 (existing one only)", len(tasks))
	}
	if tasks[0].Title != "already there" {
		t.Errorf("title = %q, want %q (pre-existing task should not be overwritten)", tasks[0].Title, "already there")
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_EnrichesURLTitledTasks(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	stub, err := env.tasks.Create("https://github.com/acme/pet1/issues/5", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env.fetcher.syncIssuesToTasks([]github.Issue{{
		Number:     5,
		Title:      "real title",
		Body:       "real body",
		URL:        "https://github.com/acme/pet1/issues/5",
		Repository: "acme/pet1",
	}})

	tasks, err := env.tasks.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1 (enriched, not duplicated)", len(tasks))
	}
	got, err := env.tasks.Get(stub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "real title" {
		t.Errorf("Title = %q, want %q", got.Title, "real title")
	}
	if got.Issue != "https://github.com/acme/pet1/issues/5" {
		t.Errorf("Issue = %q, want enriched URL", got.Issue)
	}
	if got.ProjectID != "acme/pet1" {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, "acme/pet1")
	}
}

// TestIssuesFetcher_CrossMachineRouting_PetAndWorkSplit verifies the
// end-to-end routing story: two machines (pet-only and work-only) point at
// the same shared project universe, and each machine's fetcher only creates
// tasks for issues in its own slice of the repos.
func TestIssuesFetcher_CrossMachineRouting_PetAndWorkSplit(t *testing.T) {
	t.Parallel()

	// Same issue stream delivered to both machines.
	allIssues := []github.Issue{
		{Number: 1, Title: "pet1 bug", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
		{Number: 2, Title: "work1 bug", URL: "https://github.com/bigco/work1/issues/2", Repository: "bigco/work1"},
		{Number: 3, Title: "work2 bug", URL: "https://github.com/bigco/work2/issues/3", Repository: "bigco/work2"},
	}

	petEnv := newIssuesFetcherForTest(
		t,
		func(pt project.ProjectType) bool { return pt == project.ProjectTypePet },
		nil,
	)
	workEnv := newIssuesFetcherForTest(
		t,
		func(pt project.ProjectType) bool { return pt == project.ProjectTypeWork },
		nil,
	)

	// Both machines see the same registered projects.
	for _, dir := range []string{petEnv.projectsDir, workEnv.projectsDir} {
		writeProject(t, dir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
		writeProject(t, dir, "bigco--work1.yaml", "bigco/work1", "bigco", "work1", project.ProjectTypeWork)
		writeProject(t, dir, "bigco--work2.yaml", "bigco/work2", "bigco", "work2", project.ProjectTypeWork)
	}

	petEnv.fetcher.syncIssuesToTasks(allIssues)
	workEnv.fetcher.syncIssuesToTasks(allIssues)

	assertStringSetEqual(t, taskIssueURLs(t, petEnv.tasks), []string{
		"https://github.com/acme/pet1/issues/1",
	})
	assertStringSetEqual(t, taskIssueURLs(t, workEnv.tasks), []string{
		"https://github.com/bigco/work1/issues/2",
		"https://github.com/bigco/work2/issues/3",
	})
}

func taskIssueURLs(t *testing.T, tm *task.Manager) []string {
	t.Helper()
	tasks, err := tm.List()
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	out := make([]string, 0, len(tasks))
	for i := range tasks {
		if tasks[i].Issue != "" {
			out = append(out, tasks[i].Issue)
		}
	}
	return out
}

func assertStringSetEqual(t *testing.T, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Fatalf("len = %d, want %d: got=%v want=%v", len(g), len(w), got, want)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("index %d: got %q, want %q (got=%v want=%v)", i, g[i], w[i], got, want)
		}
	}
}
