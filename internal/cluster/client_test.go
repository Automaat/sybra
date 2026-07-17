package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func mustClient(t *testing.T, node Node) *Client {
	t.Helper()
	c, err := NewClient(node, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil client")
	}
	return c
}

type stubFollower struct {
	token    string
	tasks    []task.Task
	gotBody  []byte
	gotPath  string
	gotAuth  string
	assigned *task.Task
}

func (s *stubFollower) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		s.gotPath = r.URL.Path
		s.gotAuth = r.Header.Get("Authorization")
		s.gotBody, _ = io.ReadAll(r.Body)
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized","code":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/api/TaskService/ListTasks":
			_ = json.NewEncoder(w).Encode(s.tasks)
		case "/api/TaskService/GetTask":
			_ = json.NewEncoder(w).Encode(s.tasks[0])
		case "/api/TaskService/AssignTask":
			var args []task.Task
			_ = json.Unmarshal(s.gotBody, &args)
			if len(args) == 1 {
				s.assigned = &args[0]
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"unknown","code":"not_found"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientListTasks(t *testing.T) {
	stub := &stubFollower{token: "sekret", tasks: []task.Task{{ID: "a"}, {ID: "b"}}}
	srv := stub.server(t)
	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "sekret"})
	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("ListTasks = %+v", got)
	}
	if stub.gotAuth != "Bearer sekret" {
		t.Errorf("auth header = %q", stub.gotAuth)
	}
}

func TestClientOversizedResponseErrorsInsteadOfTruncating(t *testing.T) {
	old := maxResponseBody
	maxResponseBody = 100
	t.Cleanup(func() { maxResponseBody = old })

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/TaskService/ListTasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 200)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}})
	_, err := client.ListTasks(context.Background())
	if err == nil {
		t.Fatal("ListTasks succeeded on an oversized response, want a truncation error")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %q, want a named truncation error rather than a generic JSON parse failure", err.Error())
	}
}

func TestClientResponseUnderCapSucceeds(t *testing.T) {
	old := maxResponseBody
	maxResponseBody = 4096
	t.Cleanup(func() { maxResponseBody = old })

	stub := &stubFollower{tasks: []task.Task{{ID: "a"}}}
	srv := stub.server(t)
	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}})
	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks under the cap should succeed, got: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("ListTasks = %+v", got)
	}
}

func TestClientAssignTaskEncodesArgs(t *testing.T) {
	stub := &stubFollower{token: "sekret"}
	srv := stub.server(t)
	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "sekret"})
	if err := client.AssignTask(context.Background(), task.Task{ID: "z", Title: "hi"}); err != nil {
		t.Fatal(err)
	}
	if stub.assigned == nil || stub.assigned.ID != "z" || stub.assigned.Title != "hi" {
		t.Fatalf("assigned = %+v", stub.assigned)
	}
	if !strings.HasPrefix(string(stub.gotBody), "[") {
		t.Errorf("body must be a JSON array, got %s", stub.gotBody)
	}
}

func TestClientTokenRejection(t *testing.T) {
	stub := &stubFollower{token: "right"}
	srv := stub.server(t)
	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "wrong"})
	_, err := client.ListTasks(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.Status)
	}
}

func TestClientEndpointFailover(t *testing.T) {
	stub := &stubFollower{token: "", tasks: []task.Task{{ID: "ok"}}}
	live := stub.server(t)

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{deadURL, live.URL}})
	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("failover did not reach the live endpoint: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("ListTasks after failover = %+v", got)
	}
	if client.ActiveEndpoint() != live.URL {
		t.Errorf("active endpoint = %q, want %q (should re-select the reachable one)", client.ActiveEndpoint(), live.URL)
	}
}

func TestClientAllEndpointsUnreachable(t *testing.T) {
	d1 := httptest.NewServer(http.NewServeMux())
	u1 := d1.URL
	d1.Close()
	d2 := httptest.NewServer(http.NewServeMux())
	u2 := d2.URL
	d2.Close()

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{u1, u2}})
	_, err := client.ListTasks(context.Background())
	if err == nil {
		t.Fatal("want error when all endpoints are unreachable")
	}
	if !strings.Contains(err.Error(), "all 2 endpoints failed") {
		t.Errorf("err = %v, want 'all 2 endpoints failed'", err)
	}
}

func TestClientNoEndpoints(t *testing.T) {
	client := mustClient(t, Node{Name: "empty", Endpoints: []string{"", "  "}})
	if _, err := client.ListTasks(context.Background()); err == nil {
		t.Fatal("want error for a node with no usable endpoints")
	}
}
