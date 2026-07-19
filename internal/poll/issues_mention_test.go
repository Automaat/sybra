package poll

import (
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
)

func TestIssuesFetcher_SyncMentionedIssuesToTasks_DisabledByDefault(t *testing.T) {
	t.Parallel()

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	asked := false
	env.fetcher.fetchMentioned = func(repos []string, phrase string) ([]github.Issue, error) {
		asked = true
		return nil, nil
	}

	// mentionTrigger left unset (empty) — the default.
	env.fetcher.syncMentionedIssuesToTasks()

	if asked {
		t.Fatal("fetchMentioned was called with an empty mention trigger, want no call")
	}
}

func TestIssuesFetcher_SyncMentionedIssuesToTasks_CreatesTask(t *testing.T) {
	t.Parallel()

	mentioned := []github.Issue{
		{Number: 5, Title: "mentioned issue", URL: "https://github.com/acme/pet1/issues/5", Repository: "acme/pet1"},
	}

	env := newIssuesFetcherForTest(t, nil, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)

	var askedRepos []string
	var askedPhrase string
	env.fetcher.fetchMentioned = func(repos []string, phrase string) ([]github.Issue, error) {
		askedRepos = append([]string(nil), repos...)
		askedPhrase = phrase
		return mentioned, nil
	}
	env.fetcher.SetMentionTrigger("@sybra")

	env.fetcher.syncMentionedIssuesToTasks()

	assertStringSetEqual(t, askedRepos, []string{"acme/pet1"})
	if askedPhrase != "@sybra" {
		t.Fatalf("phrase = %q, want %q", askedPhrase, "@sybra")
	}
	assertStringSetEqual(t, taskIssueURLs(t, env.tasks), []string{"https://github.com/acme/pet1/issues/5"})
}

func TestIssuesFetcher_SyncMentionedIssuesToTasks_RespectsProjectType(t *testing.T) {
	t.Parallel()

	mentioned := []github.Issue{
		{Number: 5, Title: "pet mention", URL: "https://github.com/acme/pet1/issues/5", Repository: "acme/pet1"},
		{Number: 6, Title: "work mention", URL: "https://github.com/bigco/work1/issues/6", Repository: "bigco/work1"},
	}

	env := newIssuesFetcherForTest(t, func(pt project.ProjectType) bool { return pt == project.ProjectTypePet }, nil)
	writeProject(t, env.projectsDir, "acme--pet1.yaml", "acme/pet1", "acme", "pet1", project.ProjectTypePet)
	writeProject(t, env.projectsDir, "bigco--work1.yaml", "bigco/work1", "bigco", "work1", project.ProjectTypeWork)

	var askedRepos []string
	env.fetcher.fetchMentioned = func(repos []string, phrase string) ([]github.Issue, error) {
		askedRepos = append([]string(nil), repos...)
		return mentioned, nil
	}
	env.fetcher.SetMentionTrigger("@sybra")

	env.fetcher.syncMentionedIssuesToTasks()

	assertStringSetEqual(t, askedRepos, []string{"acme/pet1"})
	// The mentioned-issue stub still returns the work issue too (a fake gh
	// response can't be narrowed by the closure), but syncIssuesToTasks'
	// registered-project lookup must still drop it.
	assertStringSetEqual(t, taskIssueURLs(t, env.tasks), []string{"https://github.com/acme/pet1/issues/5"})
}
