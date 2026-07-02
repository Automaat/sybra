package todoist

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListActiveTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("project_id") != "123" {
			t.Errorf("unexpected project_id: %s", r.URL.Query().Get("project_id"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[
			{"id":"1","content":"Buy milk","priority":1,"labels":["errand"]},
			{"id":"2","content":"Fix bug","priority":4,"labels":["dev"],"due":{"date":"2026-04-10","is_recurring":true,"string":"every day"}}
		],"next_cursor":null}`)
	}))
	defer srv.Close()

	c := NewClientWith("test-token", srv.Client(), srv.URL)
	tasks, err := c.ListActiveTasks(context.Background(), "123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Content != "Buy milk" {
		t.Errorf("expected 'Buy milk', got %q", tasks[0].Content)
	}
	if tasks[1].Due == nil || !tasks[1].Due.IsRecurring {
		t.Error("expected task 2 to have recurring due date")
	}
}

func TestListActiveTasks_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid token"}`)
	}))
	defer srv.Close()

	c := NewClientWith("bad-token", srv.Client(), srv.URL)
	_, err := c.ListActiveTasks(context.Background(), "123")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

// TestListActiveTasks_ContextCancelled proves the request is built with
// http.NewRequestWithContext: cancelling ctx before the call must abort the
// request instead of hitting the server, and never be checked into a build
// that reverts to http.NewRequest.
func TestListActiveTasks_ContextCancelled(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewClientWith("test-token", srv.Client(), srv.URL)
	_, err := c.ListActiveTasks(ctx, "123")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in error chain, got: %v", err)
	}
	select {
	case <-called:
		t.Error("server should not have been contacted with a cancelled context")
	default:
	}
}

func TestCloseTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks/42/close" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClientWith("test-token", srv.Client(), srv.URL)
	if err := c.CloseTask(context.Background(), "42"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"id":"100","name":"Inbox"},{"id":"200","name":"Work"}],"next_cursor":null}`)
	}))
	defer srv.Close()

	c := NewClientWith("test-token", srv.Client(), srv.URL)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	if projects[1].Name != "Work" {
		t.Errorf("expected 'Work', got %q", projects[1].Name)
	}
}
