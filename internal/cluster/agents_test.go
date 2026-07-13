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

func TestAPIErrorClassifiesFollowerStatus(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		wantIsClient bool
	}{
		{"follower 400 is actionable by the caller", 400, true},
		{"follower 404 is actionable by the caller", 404, true},
		{"follower 409 is actionable by the caller", 409, true},
		{"follower 500 is not relayable", 500, false},
		{"follower 502 is not relayable", 502, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &APIError{Status: tc.status, Message: "boom"}
			if got := err.IsClientError(); got != tc.wantIsClient {
				t.Fatalf("IsClientError() = %v, want %v", got, tc.wantIsClient)
			}
		})
	}
}
