package poll

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
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

	logger := slog.New(slog.DiscardHandler)
	f := NewIssuesFetcher(taskMgr, projStore, func(string, any) {}, logger, allowsType)

	// Inject the labeled fetch so tests control the "gh" response.
	f.fetchLabeled = func([]string, string) ([]github.Issue, error) {
		return labeled, nil
	}
	// Assigned path is exercised separately; default to empty to avoid gh.
	f.fetchAssigned = func() ([]github.Issue, error) { return nil, nil }
	f.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	f.viewerLogin = func() string { return "me" }

	return &issuesFetcherEnv{
		fetcher:     f,
		tasks:       taskMgr,
		projects:    projStore,
		projectsDir: projectsDir,
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_AutoExpandsUmbrella(t *testing.T) {
	t.Parallel()
	env := newIssuesFetcherForTest(t, func(project.ProjectType) bool { return true }, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	var expanded []string
	env.fetcher.SetUmbrellaExpander(func(issueURL string) (umbrella.Result, error) {
		expanded = append(expanded, issueURL)
		return umbrella.Result{Created: 2}, nil
	})

	issues := []github.Issue{
		{Number: 1, Title: "☂️ umbrella", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
		{Number: 2, Title: "normal issue", URL: "https://github.com/acme/pet1/issues/2", Repository: "acme/pet1"},
	}
	env.fetcher.syncIssuesToTasks(issues)

	// The umbrella was expanded, not turned into a flat task.
	if len(expanded) != 1 || expanded[0] != "https://github.com/acme/pet1/issues/1" {
		t.Fatalf("expander calls = %v, want [issue 1]", expanded)
	}
	// Only the normal issue produced a flat task.
	assertStringSetEqual(t, taskIssueURLs(t, env.tasks), []string{"https://github.com/acme/pet1/issues/2"})
}

func TestIssuesFetcher_SyncIssuesToTasks_UmbrellaChildNotDuplicated(t *testing.T) {
	t.Parallel()
	env := newIssuesFetcherForTest(t, func(project.ProjectType) bool { return true }, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	const subURL = "https://github.com/acme/pet1/issues/5"
	// Simulate Expand creating a gated child task for sub-issue #5.
	env.fetcher.SetUmbrellaExpander(func(issueURL string) (umbrella.Result, error) {
		_, err := env.tasks.CreateFull("child 5", "", task.AgentModeHeadless, task.Update{
			Issue:         task.Ptr(subURL),
			UmbrellaIssue: task.Ptr(issueURL),
			Status:        task.Ptr(task.StatusTodo),
			Tags:          task.Ptr([]string{umbrella.GatedTag}),
		})
		return umbrella.Result{Created: 1}, err
	})

	// Umbrella and its sub-issue arrive in the SAME batch.
	issues := []github.Issue{
		{Number: 1, Title: "☂️ umbrella", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
		{Number: 5, Title: "sub issue 5", URL: subURL, Repository: "acme/pet1"},
	}
	env.fetcher.syncIssuesToTasks(issues)

	// Sub-issue #5 must have exactly one task — the gated child — not a second
	// ungated flat todo task that would bypass the dependency DAG.
	all, err := env.tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	matches := 0
	for i := range all {
		if all[i].Issue == subURL {
			matches++
			if all[i].Status != task.StatusTodo {
				t.Fatalf("sub-issue task status = %q, want todo (gated child)", all[i].Status)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("sub-issue #5 has %d tasks, want 1 (no duplicate flat task)", matches)
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_UmbrellaDisabledIsFlat(t *testing.T) {
	t.Parallel()
	env := newIssuesFetcherForTest(t, func(project.ProjectType) bool { return true }, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
	// No expander set → feature disabled → umbrella becomes an ordinary task.

	issues := []github.Issue{
		{Number: 1, Title: "☂️ umbrella", URL: "https://github.com/acme/pet1/issues/1", Repository: "acme/pet1"},
	}
	env.fetcher.syncIssuesToTasks(issues)

	assertStringSetEqual(t, taskIssueURLs(t, env.tasks), []string{"https://github.com/acme/pet1/issues/1"})
}

func TestIssuesFetcher_SyncIssuesToTasks_UmbrellaRespectsProjectType(t *testing.T) {
	t.Parallel()
	// pet-only machine must not expand an umbrella in a work repo.
	env := newIssuesFetcherForTest(t, func(pt project.ProjectType) bool { return pt == project.ProjectTypePet }, nil)
	writeProject(t, env.projectsDir, "bigco--work1.yaml", "bigco/work1", "bigco", "work1", project.ProjectTypeWork)

	called := false
	env.fetcher.SetUmbrellaExpander(func(string) (umbrella.Result, error) {
		called = true
		return umbrella.Result{}, nil
	})

	issues := []github.Issue{
		{Number: 1, Title: "☂️ work umbrella", URL: "https://github.com/bigco/work1/issues/1", Repository: "bigco/work1"},
	}
	env.fetcher.syncIssuesToTasks(issues)

	if called {
		t.Fatal("expander ran for a work umbrella on a pet-only machine")
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
		// task. Issues from unregistered repos are always dropped.
		wantIssues []string
	}{
		{
			name:       "pet-only machine skips work and unregistered repos",
			allowsType: petOnly,
			wantIssues: []string{
				"https://github.com/acme/pet1/issues/1",
				"https://github.com/acme/pet2/issues/2",
			},
		},
		{
			name:       "work-only machine skips pet and unregistered repos",
			allowsType: workOnly,
			wantIssues: []string{
				"https://github.com/bigco/work1/issues/3",
			},
		},
		{
			name:       "allow-all accepts every registered repo but drops unregistered",
			allowsType: allowAll,
			wantIssues: []string{
				"https://github.com/acme/pet1/issues/1",
				"https://github.com/acme/pet2/issues/2",
				"https://github.com/bigco/work1/issues/3",
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
				if label != "sybra" {
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

func TestIssuesFetcher_SyncIssuesToTasks_LinkedViewerPR(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
	env.fetcher.fetchIssueLinkedPRs = func(repo string, issueNumber int) ([]github.PullRequest, error) {
		if repo != "acme/pet1" || issueNumber != 6 {
			t.Fatalf("linked PR fetch = %s#%d, want acme/pet1#6", repo, issueNumber)
		}
		return []github.PullRequest{{
			Number:      42,
			HeadRefName: "fix/issue-6",
			Author:      "me",
		}}, nil
	}

	env.fetcher.syncIssuesToTasks([]github.Issue{{
		Number:     6,
		Title:      "linked issue",
		URL:        "https://github.com/acme/pet1/issues/6",
		Repository: "acme/pet1",
	}})

	got := onlyTask(t, env.tasks)
	if got.Status != task.StatusInReview {
		t.Fatalf("Status = %q, want %q", got.Status, task.StatusInReview)
	}
	if got.PRNumber != 42 {
		t.Fatalf("PRNumber = %d, want 42", got.PRNumber)
	}
	if got.Branch != "fix/issue-6" {
		t.Fatalf("Branch = %q, want fix/issue-6", got.Branch)
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_URLTitleLinkedViewerPR(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
	stub, err := env.tasks.Create("https://github.com/acme/pet1/issues/7", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env.fetcher.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		return []github.PullRequest{{Number: 43, HeadRefName: "fix/issue-7", Author: "me"}}, nil
	}

	env.fetcher.syncIssuesToTasks([]github.Issue{{
		Number:     7,
		Title:      "real title",
		URL:        "https://github.com/acme/pet1/issues/7",
		Repository: "acme/pet1",
	}})

	tasks, err := env.tasks.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
	got, err := env.tasks.Get(stub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "real title" || got.PRNumber != 43 || got.Status != task.StatusInReview {
		t.Fatalf("task = %+v, want enriched linked viewer PR", got)
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_NoLinkedPRKeepsTodo(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	env.fetcher.syncIssuesToTasks([]github.Issue{{
		Number:     8,
		Title:      "plain issue",
		URL:        "https://github.com/acme/pet1/issues/8",
		Repository: "acme/pet1",
	}})

	got := onlyTask(t, env.tasks)
	if got.Status != task.StatusTodo {
		t.Fatalf("Status = %q, want %q", got.Status, task.StatusTodo)
	}
	if got.PRNumber != 0 || got.Branch != "" {
		t.Fatalf("linked PR fields = %d/%q, want empty", got.PRNumber, got.Branch)
	}
}

func TestIssuesFetcher_SyncIssuesToTasks_AmbiguousViewerPRsKeepTodo(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
	env.fetcher.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		return []github.PullRequest{
			{Number: 44, HeadRefName: "one", Author: "me"},
			{Number: 45, HeadRefName: "two", Author: "me"},
		}, nil
	}

	env.fetcher.syncIssuesToTasks([]github.Issue{{
		Number:     9,
		Title:      "ambiguous issue",
		URL:        "https://github.com/acme/pet1/issues/9",
		Repository: "acme/pet1",
	}})

	got := onlyTask(t, env.tasks)
	if got.Status != task.StatusTodo {
		t.Fatalf("Status = %q, want %q", got.Status, task.StatusTodo)
	}
	if got.PRNumber != 0 || got.Branch != "" {
		t.Fatalf("linked PR fields = %d/%q, want empty", got.PRNumber, got.Branch)
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

func TestIssuesFetcher_Poll_CircuitBreaksOnRepeatedAuthFailure(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	env.fetcher.fetchSnapshot = func([]string, string) (github.IssueSnapshot, error) {
		return github.IssueSnapshot{}, errors.New("gh: HTTP 401: Bad credentials")
	}

	ctx := context.Background()
	for i := range AuthFailureThreshold - 1 {
		env.fetcher.Poll(ctx)
		if env.fetcher.AuthCircuitOpen() {
			t.Fatalf("circuit opened after %d polls, want threshold %d", i+1, AuthFailureThreshold)
		}
	}

	next := env.fetcher.Poll(ctx)
	if !env.fetcher.AuthCircuitOpen() {
		t.Fatalf("circuit did not open after %d consecutive auth failures", AuthFailureThreshold)
	}
	if next != AuthCircuitBackoff {
		t.Errorf("Poll() interval = %v, want AuthCircuitBackoff (%v)", next, AuthCircuitBackoff)
	}

	// A subsequent success closes the breaker.
	env.fetcher.fetchSnapshot = func([]string, string) (github.IssueSnapshot, error) {
		return github.IssueSnapshot{}, nil
	}
	env.fetcher.Poll(ctx)
	if env.fetcher.AuthCircuitOpen() {
		t.Error("circuit stayed open after a successful poll")
	}
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

func onlyTask(t *testing.T, tm *task.Manager) task.Task {
	t.Helper()
	tasks, err := tm.List()
	if err != nil {
		t.Fatalf("tasks.List: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
	return tasks[0]
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
