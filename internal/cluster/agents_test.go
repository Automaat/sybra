package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAgentsReturnsFollowerAgents(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/AgentService/ListAgents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "a1", "taskId": "t1", "state": "running"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}})
	agents, err := c.ListAgents(t.Context())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].ID != "a1" {
		t.Fatalf("want one agent a1, got %+v", agents)
	}
}

func TestAPIErrorRelaysFollowerClientErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{"follower 400 passes through", 400, 400},
		{"follower 404 passes through", 404, 404},
		{"follower 500 is not surfaced verbatim", 500, 500},
		{"follower 502 collapses to 500", 502, 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &APIError{Status: tc.status, Message: "boom"}
			if got := err.HTTPStatus(); got != tc.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tc.wantStatus)
			}
		})
	}
}
