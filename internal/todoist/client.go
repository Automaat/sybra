package todoist

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const defaultBaseURL = "https://api.todoist.com/rest/v2"

// HTTPDoer abstracts HTTP execution for testing.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client talks to the Todoist REST API v2.
type Client struct {
	token   string
	doer    HTTPDoer
	baseURL string
}

// NewClient creates a Client using the default http.Client.
func NewClient(token string) *Client {
	return &Client{token: token, doer: http.DefaultClient, baseURL: defaultBaseURL}
}

// NewClientWith creates a Client with a custom HTTPDoer (for testing).
func NewClientWith(token string, doer HTTPDoer, baseURL string) *Client {
	return &Client{token: token, doer: doer, baseURL: baseURL}
}

// ListActiveTasks returns all active (non-completed) tasks in the given project.
func (c *Client) ListActiveTasks(projectID string) ([]Task, error) {
	url := fmt.Sprintf("%s/tasks?project_id=%s", c.baseURL, projectID)
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("todoist list tasks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("todoist list tasks: HTTP %d: %s", resp.StatusCode, body)
	}

	var tasks []Task
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	return tasks, nil
}

// CloseTask marks a Todoist task as completed.
func (c *Client) CloseTask(todoistID string) error {
	url := fmt.Sprintf("%s/tasks/%s/close", c.baseURL, todoistID)
	req, err := http.NewRequest(http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.doer.Do(req)
	if err != nil {
		return fmt.Errorf("todoist close task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("todoist close task: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// ListProjects returns all projects visible to the authenticated user.
func (c *Client) ListProjects() ([]Project, error) {
	url := fmt.Sprintf("%s/projects", c.baseURL)
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("todoist list projects: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("todoist list projects: HTTP %d: %s", resp.StatusCode, body)
	}

	var projects []Project
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return projects, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
}
