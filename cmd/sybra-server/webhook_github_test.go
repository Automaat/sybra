package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

type githubWebhookFixture struct {
	action             string
	command            string
	association        string
	userType           string
	repo               string
	number             int
	title              string
	description        string
	url                string
	commentID          int64
	installationID     int64
	includePullRequest bool
}

func githubWebhookBody(t *testing.T, fixture githubWebhookFixture) []byte {
	t.Helper()
	if fixture.action == "" {
		fixture.action = githubCommentAction
	}
	if fixture.association == "" {
		fixture.association = "MEMBER"
	}
	if fixture.userType == "" {
		fixture.userType = "User"
	}
	if fixture.repo == "" {
		fixture.repo = "example-org/example-repo"
	}
	if fixture.number == 0 {
		fixture.number = 42
	}
	if fixture.title == "" {
		fixture.title = "Make the change"
	}
	if fixture.url == "" {
		fixture.url = "https://github.com/" + fixture.repo + "/issues/42"
		if fixture.includePullRequest {
			fixture.url = "https://github.com/" + fixture.repo + "/pull/42"
		}
	}
	if fixture.commentID == 0 {
		fixture.commentID = 991
	}
	if fixture.installationID == 0 {
		fixture.installationID = 77
	}

	issue := map[string]any{
		"number":   fixture.number,
		"title":    fixture.title,
		"body":     fixture.description,
		"html_url": fixture.url,
	}
	if fixture.includePullRequest {
		issue["pull_request"] = map[string]any{"url": "https://api.github.com/repos/example-org/example-repo/pulls/42"}
	}
	payload := map[string]any{
		"action": fixture.action,
		"comment": map[string]any{
			"id":                 fixture.commentID,
			"body":               fixture.command,
			"author_association": fixture.association,
			"user": map[string]any{
				"login": "trusted-user",
				"type":  fixture.userType,
			},
		},
		"issue": issue,
		"repository": map[string]any{
			"full_name": fixture.repo,
		},
		"installation": map[string]any{
			"id": fixture.installationID,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal GitHub webhook fixture: %v", err)
		panic("unreachable")
	}
	return body
}

func serveGitHubWebhook(
	t *testing.T,
	cfg config.GitHubConfig,
	creator *fakeWebhookTaskCreator,
	event string,
	body []byte,
	signingSecret string,
) *httptest.ResponseRecorder {
	t.Helper()
	handler := newWebhookHandlerWithGitHub(testLogger(), "", cfg, creator, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set(githubEventHeader, event)
	req.Header.Set(githubSignatureHeader, webhookSignature(signingSecret, body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func githubWebhookConfig(prefix string) config.GitHubConfig {
	return config.GitHubConfig{
		Enabled: true,
		Webhook: config.GitHubWebhookConfig{
			Secret:        "github-secret",
			CommandPrefix: prefix,
		},
		App: config.GitHubAppConfig{
			InstallationID: 77,
		},
	}
}

func TestGitHubWebhookCreatesPRReviewTaskWithCustomPrefix(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-review"}}
	cfg := githubWebhookConfig("/reviewer")
	body := githubWebhookBody(t, githubWebhookFixture{
		command:            "/reviewer review",
		description:        "Please inspect the implementation.",
		includePullRequest: true,
	})

	rr := serveGitHubWebhook(t, cfg, creator, githubEventIssueComment, body, "github-secret")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if creator.calls != 1 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 1", creator.calls)
	}
	if creator.gotTitle != "Review PR #42 in example-org/example-repo" {
		t.Fatalf("title = %q", creator.gotTitle)
	}
	if creator.gotBody != "https://github.com/example-org/example-repo/pull/42\n\nPlease inspect the implementation." {
		t.Fatalf("body = %q", creator.gotBody)
	}
	if creator.gotMode != task.AgentModeHeadless {
		t.Fatalf("mode = %q, want headless", creator.gotMode)
	}
	if creator.gotInit.ProjectID == nil || *creator.gotInit.ProjectID != "example-org/example-repo" {
		t.Fatalf("project_id = %v", creator.gotInit.ProjectID)
		panic("unreachable")
	}
	if creator.gotInit.PRNumber == nil || *creator.gotInit.PRNumber != 42 {
		t.Fatalf("pr_number = %v", creator.gotInit.PRNumber)
		panic("unreachable")
	}
	if creator.gotInit.Tags == nil {
		t.Fatal("tags = nil")
		panic("unreachable")
	}
	for _, want := range []string{"review", "pr-review", "github-comment:991"} {
		if !slices.Contains(*creator.gotInit.Tags, want) {
			t.Fatalf("tags = %v, missing %q", *creator.gotInit.Tags, want)
		}
	}
}

func TestGitHubWebhookCreatesIssueImplementationTask(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-ship"}}
	cfg := githubWebhookConfig("/sybra")
	body := githubWebhookBody(t, githubWebhookFixture{
		command:     "/sybra ship",
		title:       "Handle the edge case",
		description: "Expected behavior and acceptance criteria.",
	})

	rr := serveGitHubWebhook(t, cfg, creator, githubEventIssueComment, body, "github-secret")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if creator.gotTitle != "Implement issue #42: Handle the edge case" {
		t.Fatalf("title = %q", creator.gotTitle)
	}
	if creator.gotInit.Issue == nil || *creator.gotInit.Issue != "https://github.com/example-org/example-repo/issues/42" {
		t.Fatalf("issue = %v", creator.gotInit.Issue)
		panic("unreachable")
	}
	if creator.gotInit.PRNumber != nil {
		t.Fatalf("pr_number = %v, want nil", creator.gotInit.PRNumber)
		panic("unreachable")
	}
	if creator.gotInit.Tags == nil || !slices.Equal(*creator.gotInit.Tags, []string{"ship-issue", "github-comment:991"}) {
		t.Fatalf("tags = %v", creator.gotInit.Tags)
	}
}

func TestGitHubWebhookRejectsInvalidSignature(t *testing.T) {
	creator := &fakeWebhookTaskCreator{}
	cfg := githubWebhookConfig("/sybra")
	body := githubWebhookBody(t, githubWebhookFixture{command: "/sybra ship"})

	rr := serveGitHubWebhook(t, cfg, creator, githubEventIssueComment, body, "wrong-secret")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if creator.calls != 0 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
	}
}

func TestGitHubWebhookDoesNotExposeUnsignedTaskRoute(t *testing.T) {
	creator := &fakeWebhookTaskCreator{}
	handler := newWebhookHandlerWithGitHub(testLogger(), "", githubWebhookConfig("/sybra"), creator, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(`{"title":"unsigned"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if creator.calls != 0 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
	}
}

func TestGitHubWebhookPreservesExplicitUnsignedTaskRoute(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-legacy"}}
	cfg := githubWebhookConfig("/sybra")
	cfg.Webhook.TaskEnabled = true
	handler := newWebhookHandlerWithGitHub(testLogger(), "", cfg, creator, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/task", strings.NewReader(`{"title":"legacy task"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if creator.calls != 1 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 1", creator.calls)
	}
}

func TestGitHubWebhookIgnoresNonMatchingDeliveries(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		fixture githubWebhookFixture
		mutate  func(*config.GitHubConfig)
	}{
		{
			name:    "unhandled event",
			event:   "push",
			fixture: githubWebhookFixture{command: "/sybra ship"},
		},
		{
			name:    "unhandled action",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{action: "edited", command: "/sybra ship"},
		},
		{
			name:    "wrong prefix",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/other ship"},
		},
		{
			name:    "extra command text",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra ship now"},
		},
		{
			name:    "outside contributor",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra ship", association: "CONTRIBUTOR"},
		},
		{
			name:    "bot commenter",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra ship", userType: "Bot"},
		},
		{
			name:    "review command on issue",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra review"},
		},
		{
			name:  "ship command on pull request",
			event: githubEventIssueComment,
			fixture: githubWebhookFixture{
				command:            "/sybra ship",
				includePullRequest: true,
			},
		},
		{
			name:    "wrong installation",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra ship", installationID: 88},
		},
		{
			name:    "integration disabled",
			event:   githubEventIssueComment,
			fixture: githubWebhookFixture{command: "/sybra ship"},
			mutate: func(cfg *config.GitHubConfig) {
				cfg.Enabled = false
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creator := &fakeWebhookTaskCreator{}
			cfg := githubWebhookConfig("/sybra")
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			body := githubWebhookBody(t, tc.fixture)

			rr := serveGitHubWebhook(t, cfg, creator, tc.event, body, "github-secret")

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
			}
			if creator.calls != 0 {
				t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
			}
		})
	}
}

func TestGitHubWebhookDeduplicatesComment(t *testing.T) {
	creator := &fakeWebhookTaskCreator{
		listed: []task.Task{{
			ID:   "existing-task",
			Tags: []string{"ship-issue", "github-comment:991"},
		}},
	}
	cfg := githubWebhookConfig("/sybra")
	body := githubWebhookBody(t, githubWebhookFixture{command: "/sybra ship"})

	rr := serveGitHubWebhook(t, cfg, creator, githubEventIssueComment, body, "github-secret")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if creator.calls != 0 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
	}
	var response webhookTaskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
		panic("unreachable")
	}
	if response.TaskID != "existing-task" {
		t.Fatalf("task_id = %q, want existing-task", response.TaskID)
	}
}

func TestParseGitHubCommentCommand(t *testing.T) {
	tests := []struct {
		body   string
		prefix string
		want   string
		ok     bool
	}{
		{body: "/sybra ship", prefix: "/sybra", want: "ship", ok: true},
		{body: "  /Custom REVIEW\n", prefix: "/custom", want: "review", ok: true},
		{body: "/sybra", prefix: "/sybra"},
		{body: "/sybra review later", prefix: "/sybra"},
		{body: "please /sybra review", prefix: "/sybra"},
		{body: "/other review", prefix: "/sybra"},
	}

	for _, tc := range tests {
		t.Run(tc.body, func(t *testing.T) {
			got, ok := parseGitHubCommentCommand(tc.body, tc.prefix)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseGitHubCommentCommand(%q, %q) = (%q, %v), want (%q, %v)",
					tc.body, tc.prefix, got, ok, tc.want, tc.ok)
			}
		})
	}
}
