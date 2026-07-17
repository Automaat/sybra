package cluster_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/task"
)

type fakeTaskService struct {
	tasks    []task.Task
	assigned []task.Task
}

func (f *fakeTaskService) ListTasks() ([]task.Task, error) { return f.tasks, nil }

func (f *fakeTaskService) GetTask(id string) (task.Task, error) {
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			return f.tasks[i], nil
		}
	}
	return task.Task{}, nil
}

func (f *fakeTaskService) AssignTask(t task.Task) error {
	f.assigned = append(f.assigned, t)
	return nil
}

func realServer(t *testing.T, token string, svc *fakeTaskService) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(svc, "ListTasks", "GetTask", "AssignTask"),
	}, slog.Default())

	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			mux.ServeHTTP(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(authed)
	t.Cleanup(srv.Close)
	return srv
}

func mustClient(t *testing.T, node cluster.Node) *cluster.Client {
	t.Helper()
	c, err := cluster.NewClient(node, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil client")
	}
	return c
}

func TestClientAgainstRealHTTPAPI(t *testing.T) {
	svc := &fakeTaskService{tasks: []task.Task{{ID: "t1", Title: "one"}, {ID: "t2"}}}
	srv := realServer(t, "tok", svc)

	client := mustClient(t, cluster.Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "tok"})

	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks against real httpapi: %v", err)
	}
	if len(got) != 2 || got[0].ID != "t1" {
		t.Fatalf("ListTasks = %+v", got)
	}

	one, err := client.GetTask(context.Background(), "t1")
	if err != nil || one.Title != "one" {
		t.Fatalf("GetTask = %+v, err %v", one, err)
	}

	if err := client.AssignTask(context.Background(), task.Task{ID: "t3"}); err != nil {
		t.Fatalf("AssignTask against real httpapi: %v", err)
	}
	if len(svc.assigned) != 1 || svc.assigned[0].ID != "t3" {
		t.Fatalf("server did not receive the assigned task: %+v", svc.assigned)
	}

	endpoint, degraded, err := client.ProbeHealth(context.Background())
	if err != nil || degraded || endpoint != srv.URL {
		t.Fatalf("ProbeHealth endpoint=%q degraded=%v err=%v", endpoint, degraded, err)
	}
}

func TestClientTokenRejectedByRealServer(t *testing.T) {
	svc := &fakeTaskService{}
	srv := realServer(t, "right", svc)
	client := mustClient(t, cluster.Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "wrong"})
	if _, err := client.ListTasks(context.Background()); err == nil {
		t.Fatal("real server must reject a wrong token")
	}
}
