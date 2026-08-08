package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// recordingServer stands in for a running sybra-server: it records the method
// each call landed on and replies with a canned body.
type recordingServer struct {
	*httptest.Server
	paths []string
}

func newRecordingServer(t *testing.T, reply any) *recordingServer {
	t.Helper()
	rec := &recordingServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.paths = append(rec.paths, r.URL.Path)
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (rec *recordingServer) client() *apiClient {
	return &apiClient{baseURL: rec.URL, token: "test-token", http: rec.Client()}
}

func TestAPITaskBoard_SendsEveryOperationToTheServer(t *testing.T) {
	tests := []struct {
		name   string
		call   func(b *apiTaskBoard) error
		method string
		reply  any
	}{
		{"list", func(b *apiTaskBoard) error { _, err := b.List(); return err }, "ListTasks", []task.Task{}},
		{"get", func(b *apiTaskBoard) error { _, err := b.Get("t1"); return err }, "GetTask", map[string]any{}},
		{"create", func(b *apiTaskBoard) error { _, err := b.Create("t", "", "headless"); return err }, "CreateTask", map[string]any{}},
		{
			"create with status",
			func(b *apiTaskBoard) error {
				_, err := b.CreateWithStatus("t", "", "headless", task.StatusTodo, task.Update{})
				return err
			},
			"CreateTaskFull",
			map[string]any{},
		},
		{"update map", func(b *apiTaskBoard) error { _, err := b.UpdateMap("t1", nil); return err }, "UpdateTask", map[string]any{}},
		{"update fields", func(b *apiTaskBoard) error { _, err := b.Update("t1", task.Update{}); return err }, "UpdateTaskFields", map[string]any{}},
		{
			"apply",
			func(b *apiTaskBoard) error {
				_, err := b.Apply(task.TransitionIntent{TaskID: "t1"})
				return err
			},
			"ApplyTransition",
			map[string]any{},
		},
		{"touch", func(b *apiTaskBoard) error { _, err := b.Touch("t1"); return err }, "TouchTask", map[string]any{}},
		{"delete", func(b *apiTaskBoard) error { return b.Delete("t1") }, "DeleteTask", map[string]any{}},
		{"list trash", func(b *apiTaskBoard) error { _, err := b.ListTrash(); return err }, "ListTrash", []task.TrashEntry{}},
		{"restore", func(b *apiTaskBoard) error { _, err := b.RestoreFromTrash("t1"); return err }, "RestoreFromTrash", map[string]any{}},
		{
			"delete generation",
			func(b *apiTaskBoard) error { _, err := b.DeleteTrashedGeneration("t1"); return err },
			"DeleteTrashedGeneration",
			true,
		},
		{"prune", func(b *apiTaskBoard) error { _, err := b.PruneAllTrash(); return err }, "PruneAllTrash", map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingServer(t, tt.reply)
			if err := tt.call(newAPITaskBoard(rec.client())); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			want := "/api/TaskService/" + tt.method
			if len(rec.paths) != 1 || rec.paths[0] != want {
				t.Errorf("called %v, want a single %s", rec.paths, want)
			}
		})
	}
}

func TestAPITaskBoard_ReportsTheServersOwnReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"task t1 is owned by node home-nas","code":"conflict"}`))
	}))
	t.Cleanup(srv.Close)

	board := newAPITaskBoard(&apiClient{baseURL: srv.URL, token: "t", http: srv.Client()})
	_, err := board.Get("t1")
	if err == nil {
		t.Fatal("expected the server's rejection to surface")
	}
	// A generic "command failed" would leave the operator with nothing to act
	// on; the whole point of routing through the server is its reason.
	if !strings.Contains(err.Error(), "owned by node home-nas") {
		t.Errorf("error %q does not carry the server's reason", err)
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("error %q does not carry the server's code", err)
	}
}

func TestAPIProjectBoard_RawTypeReadsTheStoredValue(t *testing.T) {
	rec := newRecordingServer(t, map[string]any{"id": "owner/repo", "type": "work"})
	got, err := newAPIProjectBoard(rec.client()).RawType("owner/repo")
	if err != nil {
		t.Fatalf("RawType: %v", err)
	}
	if string(got) != "work" {
		t.Errorf("RawType = %q, want %q", got, "work")
	}
}

func TestCallAPI_WithoutATargetFailsLoudly(t *testing.T) {
	_, err := callAPI[task.Task](nil, taskServiceName, "GetTask", "t1")
	if err == nil {
		t.Fatal("expected an error when no server target is configured")
	}
}

func TestNewRemoteAPIClient(t *testing.T) {
	tests := []struct {
		name   string
		target string
		token  string
		want   string
	}{
		{"https target with token", "https://board.example:8443", "secret", "https://board.example:8443"},
		{"cleartext target refused", "http://board.example:8080", "secret", ""},
		{"https target without token refused", "https://board.example:8443", "", ""},
		{"path refused", "https://board.example:8443/api", "secret", ""},
		{"unset", "", "secret", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(serverTargetEnv, tt.target)
			t.Setenv(serverTokenEnv, tt.token)
			client, ok := newRemoteAPIClient()
			if tt.want == "" {
				if ok {
					t.Fatalf("expected no remote client, got %s", client.baseURL)
				}
				return
			}
			if !ok {
				t.Fatal("expected a remote client")
			}
			if client.baseURL != tt.want {
				t.Errorf("baseURL = %q, want %q", client.baseURL, tt.want)
			}
			if client.token != tt.token {
				t.Errorf("token = %q, want %q", client.token, tt.token)
			}
		})
	}
}
