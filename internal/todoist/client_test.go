package todoist

import (
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
	tasks, err := c.ListActiveTasks("123")
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
	_, err := c.ListActiveTasks("123")
	if err == nil {
		t.Fatal("expected error for 401 response")
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
	if err := c.CloseTask("42"); err != nil {
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
	projects, err := c.ListProjects()
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
