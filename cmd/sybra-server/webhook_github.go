package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/task"
)

const (
	githubEventHeader       = "X-GitHub-Event"
	githubSignatureHeader   = "X-Hub-Signature-256"
	githubEventIssueComment = "issue_comment"
	githubCommentAction     = "created"
	githubCommandPrefix     = "/sybra"
	githubCommandShip       = "ship"
	githubCommandReview     = "review"
	githubCommentTagPrefix  = "github-comment:"
)

type githubIssueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID                int64  `json:"id"`
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
		User              struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number      int             `json:"number"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		HTMLURL     string          `json:"html_url"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type githubWebhookTask struct {
	title string
	body  string
	init  task.Update
	tag   string
}

func newGitHubWebhookHandler(
	logger *slog.Logger,
	cfg config.GitHubConfig,
	secret string,
	creator webhookTaskCreator,
	admit webhookAdmissionFunc,
) http.Handler {
	var createMu sync.Mutex

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWebhookError(w, logger, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if !cfg.Enabled || strings.TrimSpace(secret) == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, httpapi.MaxRequestBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeWebhookError(w, logger, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
				return
			}
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", "failed to read request body")
			return
		}
		if !validWebhookSignature(secret, r.Header.Get(githubSignatureHeader), body) {
			writeWebhookError(w, logger, http.StatusUnauthorized, "unauthorized", "invalid GitHub webhook signature")
			return
		}
		if r.Header.Get(githubEventHeader) != githubEventIssueComment {
			w.WriteHeader(http.StatusOK)
			return
		}

		var payload githubIssueCommentPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", "invalid JSON payload")
			return
		}
		if payload.Action != githubCommentAction ||
			!trustedGitHubCommentAuthor(payload.Comment.AuthorAssociation, payload.Comment.User.Type) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if cfg.App.InstallationID > 0 && payload.Installation.ID != cfg.App.InstallationID {
			w.WriteHeader(http.StatusOK)
			return
		}

		command, ok := parseGitHubCommentCommand(payload.Comment.Body)
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		isPullRequest := len(payload.Issue.PullRequest) > 0 && string(payload.Issue.PullRequest) != "null"
		if (command == githubCommandReview) != isPullRequest {
			w.WriteHeader(http.StatusOK)
			return
		}

		request, err := githubTaskFromComment(payload, command)
		if err != nil {
			writeWebhookError(w, logger, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		createMu.Lock()
		defer createMu.Unlock()

		existing, found, err := findWebhookTaskByTag(creator, request.tag)
		if err != nil {
			logger.Error("webhook.github.dedup", "err", err)
			writeWebhookError(w, logger, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if found {
			writeWebhookJSON(w, http.StatusOK, webhookTaskResponse{TaskID: existing.ID})
			return
		}
		if admit != nil {
			if err := admit(); err != nil {
				writeWebhookAdmissionError(w, logger, err)
				return
			}
		}

		created, err := creator.CreateTaskWithInit(request.title, request.body, task.AgentModeHeadless, request.init)
		if err != nil {
			logger.Error("webhook.github.create_task", "err", err)
			writeWebhookError(w, logger, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		writeWebhookJSON(w, http.StatusCreated, webhookTaskResponse{TaskID: created.ID})
	})
}

func trustedGitHubCommentAuthor(association, userType string) bool {
	if strings.EqualFold(strings.TrimSpace(userType), "Bot") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func parseGitHubCommentCommand(body string) (string, bool) {
	fields := strings.Fields(body)
	if len(fields) != 2 || !strings.EqualFold(fields[0], githubCommandPrefix) {
		return "", false
	}
	switch command := strings.ToLower(fields[1]); command {
	case githubCommandShip, githubCommandReview:
		return command, true
	default:
		return "", false
	}
}

func githubTaskFromComment(payload githubIssueCommentPayload, command string) (githubWebhookTask, error) {
	repo := strings.TrimSpace(payload.Repository.FullName)
	if repo == "" || payload.Issue.Number <= 0 || payload.Comment.ID <= 0 {
		return githubWebhookTask{}, fmt.Errorf("GitHub payload is missing repository, issue number, or comment ID")
	}

	url := strings.TrimSpace(payload.Issue.HTMLURL)
	if url == "" {
		kind := "issues"
		if command == githubCommandReview {
			kind = "pull"
		}
		url = fmt.Sprintf("https://github.com/%s/%s/%d", repo, kind, payload.Issue.Number)
	}
	body := url
	if description := strings.TrimSpace(payload.Issue.Body); description != "" {
		body += "\n\n" + description
	}

	sourceTag := githubCommentTagPrefix + strconv.FormatInt(payload.Comment.ID, 10)
	init := task.Update{
		ProjectID: task.Ptr(repo),
	}
	var title string
	switch command {
	case githubCommandReview:
		title = fmt.Sprintf("Review PR #%d in %s", payload.Issue.Number, repo)
		init.PRNumber = task.Ptr(payload.Issue.Number)
		init.Tags = task.Ptr([]string{"review", "pr-review", sourceTag})
	case githubCommandShip:
		title = fmt.Sprintf("Implement issue #%d: %s", payload.Issue.Number, strings.TrimSpace(payload.Issue.Title))
		init.Issue = task.Ptr(url)
		init.Tags = task.Ptr([]string{"ship-issue", sourceTag})
	default:
		return githubWebhookTask{}, fmt.Errorf("unsupported GitHub command %q", command)
	}

	return githubWebhookTask{title: title, body: body, init: init, tag: sourceTag}, nil
}

func findWebhookTaskByTag(creator webhookTaskCreator, tag string) (task.Task, bool, error) {
	tasks, err := creator.ListTasks()
	if err != nil {
		return task.Task{}, false, err
	}
	for i := range tasks {
		if slices.Contains(tasks[i].Tags, tag) {
			return tasks[i], true, nil
		}
	}
	return task.Task{}, false, nil
}
