package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func resetServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMutatingRPCNoRetryAfterPartialSend(t *testing.T) {
	reset := resetServer(t)
	stub := &stubFollower{tasks: []task.Task{{ID: "ok"}}}
	live := stub.server(t)

	client, _ := NewClient(Node{Name: "n1", Endpoints: []string{reset.URL, live.URL}}, nil)

	err := client.AssignTask(context.Background(), task.Task{ID: "z"})
	if err == nil {
		t.Fatal("AssignTask should fail: the request may have been delivered to the reset endpoint")
	}
	if stub.assigned != nil {
		t.Fatalf("mutating RPC must NOT fail over after a partial send; live endpoint saw assign %+v", stub.assigned)
	}

	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("idempotent ListTasks should fail over to the live endpoint: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("ListTasks after failover = %+v", got)
	}
}

func gatewayDownServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "gateway down")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGatewayDownFailsOverForIdempotent(t *testing.T) {
	down := gatewayDownServer(t, http.StatusServiceUnavailable)
	stub := &stubFollower{tasks: []task.Task{{ID: "ok"}}}
	live := stub.server(t)

	client, _ := NewClient(Node{Name: "n1", Endpoints: []string{down.URL, live.URL}}, nil)
	got, err := client.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("503 gateway-down should fail over for idempotent read: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("ListTasks = %+v", got)
	}
}

func TestGatewayDownReturnedForMutating(t *testing.T) {
	down := gatewayDownServer(t, http.StatusBadGateway)
	stub := &stubFollower{}
	live := stub.server(t)

	client, _ := NewClient(Node{Name: "n1", Endpoints: []string{down.URL, live.URL}}, nil)
	err := client.AssignTask(context.Background(), task.Task{ID: "z"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("mutating RPC should return the 502 APIError, not fail over; got %v", err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", apiErr.Status)
	}
	if stub.assigned != nil {
		t.Error("mutating RPC must not have reached the live endpoint on a 502")
	}
}
