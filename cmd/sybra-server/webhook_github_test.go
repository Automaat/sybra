package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sybra"
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
	}
	return body
}

func serveGitHubWebhook(
	t *testing.T,
	cfg config.GitHubConfig,
	creator webhookTaskCreator,
	event string,
	body []byte,
	handlerSecret string,
	signatureSecret string,
) *httptest.ResponseRecorder {
	t.Helper()
	handler := newWebhookHandlerWithGitHub(testLogger(), handlerSecret, cfg, creator, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set(githubEventHeader, event)
	req.Header.Set(githubSignatureHeader, webhookSignature(signatureSecret, body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func githubWebhookConfig() config.GitHubConfig {
	return config.GitHubConfig{
		Enabled: true,
		App: config.GitHubAppConfig{
			InstallationID: 77,
		},
	}
}

func TestGitHubWebhookCreatesPRReviewTask(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-review"}}
	body := githubWebhookBody(t, githubWebhookFixture{
		command:            "/sybra review",
		description:        "Please inspect the implementation.",
		includePullRequest: true,
	})

	rr := serveGitHubWebhook(t, githubWebhookConfig(), creator, githubEventIssueComment, body, "github-secret", "github-secret")

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
	}
	if creator.gotInit.PRNumber == nil || *creator.gotInit.PRNumber != 42 {
		t.Fatalf("pr_number = %v", creator.gotInit.PRNumber)
	}
	if creator.gotInit.Tags == nil {
		t.Fatal("tags = nil")
	}
	for _, want := range []string{"review", "pr-review", "github-comment:991"} {
		if !slices.Contains(*creator.gotInit.Tags, want) {
			t.Fatalf("tags = %v, missing %q", *creator.gotInit.Tags, want)
		}
	}
}

func TestGitHubWebhookCreatesIssueImplementationTask(t *testing.T) {
	creator := &fakeWebhookTaskCreator{created: task.Task{ID: "task-ship"}}
	body := githubWebhookBody(t, githubWebhookFixture{
		command:     "/sybra ship",
		title:       "Handle the edge case",
		description: "Expected behavior and acceptance criteria.",
	})

	rr := serveGitHubWebhook(t, githubWebhookConfig(), creator, githubEventIssueComment, body, "github-secret", "github-secret")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	if creator.gotTitle != "Implement issue #42: Handle the edge case" {
		t.Fatalf("title = %q", creator.gotTitle)
	}
	if creator.gotInit.Issue == nil || *creator.gotInit.Issue != "https://github.com/example-org/example-repo/issues/42" {
		t.Fatalf("issue = %v", creator.gotInit.Issue)
	}
	if creator.gotInit.PRNumber != nil {
		t.Fatalf("pr_number = %v, want nil", creator.gotInit.PRNumber)
	}
	if creator.gotInit.Tags == nil || !slices.Equal(*creator.gotInit.Tags, []string{"ship-issue", "github-comment:991"}) {
		t.Fatalf("tags = %v", creator.gotInit.Tags)
	}
}

func TestGitHubWebhookRejectsInvalidSignature(t *testing.T) {
	creator := &fakeWebhookTaskCreator{}
	body := githubWebhookBody(t, githubWebhookFixture{command: "/sybra ship"})

	rr := serveGitHubWebhook(t, githubWebhookConfig(), creator, githubEventIssueComment, body, "github-secret", "wrong-secret")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if creator.calls != 0 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
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
			cfg := githubWebhookConfig()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			body := githubWebhookBody(t, tc.fixture)

			rr := serveGitHubWebhook(t, cfg, creator, tc.event, body, "github-secret", "github-secret")

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
	body := githubWebhookBody(t, githubWebhookFixture{command: "/sybra ship"})

	rr := serveGitHubWebhook(t, githubWebhookConfig(), creator, githubEventIssueComment, body, "github-secret", "github-secret")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if creator.calls != 0 {
		t.Fatalf("CreateTaskWithInit calls = %d, want 0", creator.calls)
	}
	var response webhookTaskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.TaskID != "existing-task" {
		t.Fatalf("task_id = %q, want existing-task", response.TaskID)
	}
}

func TestGitHubWebhookPersistsTaskAndDeduplicatesRedelivery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_DISABLE_WORKFLOWS", "0")

	cfg := startupLikeServerTestConfig(home)
	logger := slog.New(slog.DiscardHandler)
	app := sybra.NewApp(logger, &slog.LevelVar{}, cfg)
	if err := app.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	t.Cleanup(func() {
		app.Shutdown(context.Background())
	})

	creator, err := resolveWebhookTaskCreator(app)
	if err != nil {
		t.Fatalf("resolveWebhookTaskCreator: %v", err)
	}
	handler := newWebhookHandlerWithGitHub(logger, "github-secret", githubWebhookConfig(), creator, nil)
	body := githubWebhookBody(t, githubWebhookFixture{
		command:     "/sybra ship",
		repo:        "Automaat/sybra",
		title:       "Handle the edge case",
		description: "Expected behavior and acceptance criteria.",
	})

	firstReq := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	firstReq.Header.Set(githubEventHeader, githubEventIssueComment)
	firstReq.Header.Set(githubSignatureHeader, webhookSignature("github-secret", body))
	firstRR := httptest.NewRecorder()
	handler.ServeHTTP(firstRR, firstReq)

	if firstRR.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201: %s", firstRR.Code, firstRR.Body.String())
	}
	var first webhookTaskResponse
	if err := json.Unmarshal(firstRR.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	secondReq.Header.Set(githubEventHeader, githubEventIssueComment)
	secondReq.Header.Set(githubSignatureHeader, webhookSignature("github-secret", body))
	secondRR := httptest.NewRecorder()
	handler.ServeHTTP(secondRR, secondReq)

	if secondRR.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200: %s", secondRR.Code, secondRR.Body.String())
	}
	var second webhookTaskResponse
	if err := json.Unmarshal(secondRR.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if second.TaskID != first.TaskID {
		t.Fatalf("second task_id = %q, want %q", second.TaskID, first.TaskID)
	}

	taskSvc, ok := sybra.ServiceRegistry(app)["TaskService"].Impl.(*sybra.TaskService)
	if !ok {
		t.Fatal("TaskService impl missing")
	}
	tasks, err := taskSvc.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(tasks))
	}
	created, err := taskSvc.GetTask(first.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if created.Title != "Implement issue #42: Handle the edge case" {
		t.Fatalf("title = %q", created.Title)
	}
	if created.Body != "https://github.com/Automaat/sybra/issues/42\n\nExpected behavior and acceptance criteria." {
		t.Fatalf("body = %q", created.Body)
	}
	if created.AgentMode != task.AgentModeHeadless {
		t.Fatalf("mode = %q, want %q", created.AgentMode, task.AgentModeHeadless)
	}
	if created.ProjectID != "Automaat/sybra" {
		t.Fatalf("project_id = %q, want Automaat/sybra", created.ProjectID)
	}
	if created.Issue != "https://github.com/Automaat/sybra/issues/42" {
		t.Fatalf("issue = %q", created.Issue)
	}
	if !slices.Equal(created.Tags, []string{"ship-issue", "github-comment:991"}) {
		t.Fatalf("tags = %v, want [ship-issue github-comment:991]", created.Tags)
	}
}

func TestParseGitHubCommentCommand(t *testing.T) {
	tests := []struct {
		body string
		want string
		ok   bool
	}{
		{body: "/sybra ship", want: "ship", ok: true},
		{body: "  /SYBRA REVIEW\n", want: "review", ok: true},
		{body: "/sybra"},
		{body: "/sybra review later"},
		{body: "please /sybra review"},
		{body: "/other review"},
	}

	for _, tc := range tests {
		t.Run(tc.body, func(t *testing.T) {
			got, ok := parseGitHubCommentCommand(tc.body)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseGitHubCommentCommand(%q) = (%q, %v), want (%q, %v)",
					tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}
