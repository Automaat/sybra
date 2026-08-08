package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
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

// TestAPITaskBoard_ParsesTheServersErrorEnvelope covers the client half only.
// That the server actually emits a reason instead of a sanitized 500 is a
// property of the endpoints, verified end-to-end through the real handler in
// internal/sybra (TestBoardEndpoints_RejectionCarriesItsReason) — a stubbed
// reply here cannot observe it.
func TestAPITaskBoard_ParsesTheServersErrorEnvelope(t *testing.T) {
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

// TestAPIProjectBoard_RawTypeUsesItsOwnEndpoint pins the call, not the value.
// Reading the raw type off GetProject would pass an equality assertion on a
// record that carries a type, and still fail open on the record that matters:
// GetProject coerces an absent type to pet, which is what the confidentiality
// guard must never see. internal/sybra's
// TestGetProjectRawType_KeepsUnsetDistinctFromPet covers the server half.
func TestAPIProjectBoard_RawTypeUsesItsOwnEndpoint(t *testing.T) {
	rec := newRecordingServer(t, "work")
	got, err := newAPIProjectBoard(rec.client()).RawType("owner/repo")
	if err != nil {
		t.Fatalf("RawType: %v", err)
	}
	if string(got) != "work" {
		t.Errorf("RawType = %q, want %q", got, "work")
	}
	want := "/api/ProjectService/GetProjectRawType"
	if len(rec.paths) != 1 || rec.paths[0] != want {
		t.Errorf("called %v, want a single %s", rec.paths, want)
	}
}

// TestAPIProjectBoard_CreateWaitsForTheClone guards the CLI's exit contract:
// CreateProject returns while the clone is still running, so a caller that
// exits immediately would print success on a repo that never cloned.
func TestAPIProjectBoard_CreateWaitsForTheClone(t *testing.T) {
	rec := newRecordingServer(t, map[string]any{"id": "owner/repo", "status": "ready"})
	if _, err := newAPIProjectBoard(rec.client()).Create("https://github.com/owner/repo", "pet"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := "/api/ProjectService/CreateProjectAndClone"
	if len(rec.paths) != 1 || rec.paths[0] != want {
		t.Errorf("called %v, want a single %s", rec.paths, want)
	}
}

// TestCmdDelete_DispatchesOnce guards against a retry above a server-backed
// board: the server commits the delete, the retry gets a 404 for a task that
// is already gone, and the command reports a failure that did not happen.
func TestCmdDelete_DispatchesOnce(t *testing.T) {
	rec := newRecordingServer(t, map[string]any{})
	board := newAPITaskBoard(rec.client())

	code, _ := captureStdout(t, func() int { return cmdDelete(board, []string{"t1"}, true) })
	if code != 0 {
		t.Fatalf("cmdDelete exit = %d, want 0", code)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/api/TaskService/DeleteTask" {
		t.Errorf("called %v, want exactly one DeleteTask", rec.paths)
	}
}

// TestDispatch_RefusesToFallBackToLocalFilesForARemoteBoard is the whole point
// of the issue: editing this machine's stale copy of another machine's board
// and reporting success is the silent no-op being removed. A loopback target
// is a different case — those files are the same board — and still falls back.
func TestDispatch_RefusesToFallBackToLocalFilesForARemoteBoard(t *testing.T) {
	t.Setenv(serverTargetEnv, "https://board.invalid:8443")
	t.Setenv(serverTokenEnv, "secret")

	cfg := config.DefaultConfig()
	rawStore, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	mgr := task.NewManager(rawStore, nil)
	created, err := mgr.Create("local copy", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	code, _ := captureStdout(t, func() int {
		return dispatch("update", []string{created.ID, "--title", "edited"}, cfg, mgr, nil, true, true)
	})
	if code == 0 {
		t.Fatal("dispatch reported success against an unreachable remote board")
	}
	after, err := mgr.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Title != "local copy" {
		t.Errorf("title = %q; the local file was edited instead of the remote board", after.Title)
	}
}

// TestDispatch_ConfigStillRunsWithAnUnreachableRemoteBoard keeps the commands
// an operator reaches for when the board is down usable while it is down.
func TestDispatch_ConfigStillRunsWithAnUnreachableRemoteBoard(t *testing.T) {
	t.Setenv(serverTargetEnv, "https://board.invalid:8443")
	t.Setenv(serverTokenEnv, "secret")

	cfg := config.DefaultConfig()
	code, _ := captureStdout(t, func() int {
		return dispatch("config", []string{"dump"}, cfg, nil, nil, true, true)
	})
	if code != 0 {
		t.Errorf("config dump exit = %d, want 0 with the board down", code)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"[::1]", true},
		{"192.168.20.219", false},
		{"board.example", false},
	}
	for _, tt := range tests {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
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

// TestCmdProgressAdd_WritesThroughTheServer covers a split the local artifact
// store cannot see: the entry would land in this machine's artifacts dir while
// the task it describes lives on the board that just got touched.
func TestCmdProgressAdd_WritesThroughTheServer(t *testing.T) {
	rec := newRecordingServer(t, map[string]any{"kind": "decision", "message": "chose headless"})
	board := newAPITaskBoard(rec.client())

	code, _ := captureStdout(t, func() int {
		return cmdProgressAdd(board, nil, rec.client(), nil,
			[]string{"t1", "--kind", "decision", "--message", "chose headless"}, true)
	})
	if code != 0 {
		t.Fatalf("progress add exit = %d, want 0", code)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/api/TaskService/AppendTaskProgress" {
		t.Errorf("called %v, want a single AppendTaskProgress", rec.paths)
	}
}

// TestCmdProgressList_ReadsThroughTheServer mirrors the append: a local read
// would report an empty log for a task whose entries live on the board.
func TestCmdProgressList_ReadsThroughTheServer(t *testing.T) {
	rec := newRecordingServer(t, []map[string]any{{"kind": "progress", "message": "started"}})

	code, _ := captureStdout(t, func() int {
		return cmdProgressList(rec.client(), nil, []string{"t1"}, true)
	})
	if code != 0 {
		t.Fatalf("progress list exit = %d, want 0", code)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/api/TaskService/ListTaskProgress" {
		t.Errorf("called %v, want a single ListTaskProgress", rec.paths)
	}
}

// TestOwnStateCommandsReachTheServer pins each command that used to read this
// machine's own corpus to the endpoint that reads the board's instead. A
// regression here is silent: the command still prints a plausible answer, just
// about the wrong machine.
func TestOwnStateCommandsReachTheServer(t *testing.T) {
	tests := []struct {
		name  string
		reply any
		call  func(api *apiClient) int
		want  string
	}{
		{
			name:  "audit",
			reply: []map[string]any{},
			call:  func(api *apiClient) int { return cmdAudit(config.DefaultConfig(), api, nil, true) },
			want:  "/api/AuditService/QueryAuditEvents",
		},
		{
			name:  "stats lifecycle",
			reply: []map[string]any{},
			call: func(api *apiClient) int {
				return cmdStats(config.DefaultConfig(), api, []string{"lifecycle"}, true)
			},
			want: "/api/AuditService/QueryAuditEvents",
		},
		{
			name:  "artifact list",
			reply: []map[string]any{},
			call:  func(api *apiClient) int { return cmdArtifact(api, []string{"list", "t1"}, true) },
			want:  "/api/TaskService/ListTaskArtifactMetas",
		},
		{
			name:  "artifact get",
			reply: []byte("hello"),
			call:  func(api *apiClient) int { return cmdArtifact(api, []string{"get", "t1", "log.txt"}, true) },
			want:  "/api/TaskService/ReadTaskArtifact",
		},
		{
			name:  "artifact reindex",
			reply: map[string]any{},
			call:  func(api *apiClient) int { return cmdArtifact(api, []string{"reindex", "t1"}, true) },
			want:  "/api/TaskService/ReindexTaskArtifacts",
		},
		{
			name:  "tasks-history",
			reply: []map[string]any{},
			call:  func(api *apiClient) int { return cmdTasksHistory(config.DefaultConfig(), api, nil, true) },
			want:  "/api/TaskService/ListTaskSnapshotHistory",
		},
		{
			name:  "selfmonitor scan",
			reply: map[string]any{},
			call: func(api *apiClient) int {
				return cmdSelfmonitor(config.DefaultConfig(), api, nil, []string{"scan"}, true)
			},
			want: "/api/SelfMonitorService/GetSelfMonitorReport",
		},
		{
			name:  "selfmonitor investigate",
			reply: map[string]any{},
			call: func(api *apiClient) int {
				return cmdSelfmonitor(config.DefaultConfig(), api, nil, []string{"investigate"}, true)
			},
			want: "/api/SelfMonitorService/InvestigateSelfMonitor",
		},
		{
			name:  "selfmonitor ledger",
			reply: []map[string]any{},
			call: func(api *apiClient) int {
				return cmdSelfmonitor(config.DefaultConfig(), api, nil, []string{"ledger"}, true)
			},
			want: "/api/SelfMonitorService/ListSelfMonitorLedger",
		},
		{
			name:  "harness-evolution run",
			reply: map[string]any{"result": map[string]any{}, "filed": []any{}},
			call: func(api *apiClient) int {
				return cmdHarnessEvolution(config.DefaultConfig(), api, nil, []string{"run"}, true)
			},
			want: "/api/SelfMonitorService/RunHarnessEvolution",
		},
		{
			name:  "prompt-lab run",
			reply: map[string]any{"result": map[string]any{}, "filed": []any{}},
			call: func(api *apiClient) int {
				return cmdPromptLab(config.DefaultConfig(), api, nil, nil, []string{"run"}, true)
			},
			want: "/api/PromptLabService/RunPromptLab",
		},
		{
			name:  "evaluation scan",
			reply: map[string]any{},
			call: func(api *apiClient) int {
				return cmdEvaluation(config.DefaultConfig(), api, nil, []string{"scan"}, true)
			},
			want: "/api/StatsService/ScanEvaluation",
		},
		{
			name:  "monitor map-duplicates",
			reply: map[string]any{"fingerprint": "incident:0123456789abcdef01234567", "canonical": "https://github.com/o/r/issues/9", "duplicates": []int{10}},
			call: func(api *apiClient) int {
				return cmdMonitorMapDuplicates(config.DefaultConfig(), api,
					[]string{"--fingerprint", "incident:0123456789abcdef01234567", "--issues", "10", "--coverage", "reproduced"}, true)
			},
			want: "/api/TaskService/MapDuplicateIncidents",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingServer(t, tt.reply)
			code, _ := captureStdout(t, func() int { return tt.call(rec.client()) })
			if code != 0 {
				t.Fatalf("%s exit = %d, want 0", tt.name, code)
			}
			if len(rec.paths) != 1 || rec.paths[0] != tt.want {
				t.Errorf("called %v, want a single %s", rec.paths, tt.want)
			}
		})
	}
}
