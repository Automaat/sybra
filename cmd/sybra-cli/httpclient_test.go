package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/task"
)

type fakeTaskService struct {
	tasks *task.Manager
}

func (f *fakeTaskService) GetTask(id string) (task.Task, error) {
	return f.tasks.Get(id)
}

func (f *fakeTaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	return f.tasks.UpdateMap(id, updates)
}

func (f *fakeTaskService) CreateTask(title, body, mode string) (task.Task, error) {
	return f.tasks.Create(title, body, mode)
}

func (f *fakeTaskService) DeleteTask(id string) error {
	return f.tasks.Delete(id)
}

func startFakeAPIServer(t *testing.T, tasksDir string) string {
	t.Helper()
	rawStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeTaskService{tasks: task.NewManager(rawStore, nil)}
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(svc, "GetTask", "UpdateTask", "CreateTask", "DeleteTask"),
	}, slog.Default(), nil)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","service":"` + httpserve.ServiceMarker + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

type failingTaskService struct {
	fakeTaskService
}

func (f *failingTaskService) UpdateTask(string, map[string]any) (task.Task, error) {
	return task.Task{}, errors.New("simulated internal server failure")
}

func startFailingAPIServer(t *testing.T, tasksDir string) string {
	t.Helper()
	rawStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := &failingTaskService{fakeTaskService{tasks: task.NewManager(rawStore, nil)}}
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(svc, "GetTask", "UpdateTask", "CreateTask", "DeleteTask"),
	}, slog.Default(), nil)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","service":"` + httpserve.ServiceMarker + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
}

func lockdownDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func isolateHTTPCLITestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv("SYBRA_TASKS_DIR", "")
	t.Setenv(serverTargetEnv, "")
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", "")
}

func useDefaultHTTPCLIHome(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("SYBRA_HOME", "")
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv("SYBRA_TASKS_DIR", "")
	t.Setenv(serverTargetEnv, "")
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", "")
	return filepath.Join(config.HomeDir(), "tasks")
}

func TestGetUsesHTTPModeWhenTaskOnlyExistsOnServer(t *testing.T) {
	home := t.TempDir()
	_ = useDefaultHTTPCLIHome(t, home)

	serverTasksDir := t.TempDir()
	serverStore, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := task.NewManager(serverStore, nil).Create("api-only get target", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	code, out := runCLI(t, "--json", "get", created.ID)
	if code != 0 {
		t.Fatalf("get over HTTP mode exit %d: %s", code, out)
	}
	var got task.Task
	mustUnmarshal(t, out, &got)
	if got.ID != created.ID {
		t.Fatalf("get task ID = %q, want %q", got.ID, created.ID)
	}
}

func TestNewAPIClientPrefersVerifierTokenFile(t *testing.T) {
	t.Setenv(serverTargetEnv, "127.0.0.1:12345")
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("scoped-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", tokenPath)
	cfg := &config.Config{}
	cfg.Server.AuthToken = "full-server-token"
	cfg.Cluster.TLS.CertFile = "/operator/server.crt"
	cfg.Cluster.TLS.KeyFile = "/operator/server.key"
	client, err := newAPIClient(cfg)
	if err != nil || client.token != "scoped-token" {
		t.Fatalf("client = %+v, err=%v; want scoped file token", client, err)
	}
}

func TestNewAPIClientVerifierTokenRequiresLoopback(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("scoped-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", tokenPath)
	t.Setenv(serverTargetEnv, "192.0.2.10:12345")
	cfg := &config.Config{}
	if client, err := newAPIClient(cfg); err == nil || client != nil {
		t.Fatalf("scoped verifier credential accepted non-loopback target: %+v (err=%v)", client, err)
	}
}

func TestNewAPIClientVerifierTokenRejectsRemoteTLS(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("scoped-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", tokenPath)
	t.Setenv(serverTargetEnv, "https://board.example:8443")
	cfg := &config.Config{}
	if client, err := newAPIClient(cfg); err == nil || client != nil {
		t.Fatalf("scoped verifier credential accepted remote TLS target: %+v (err=%v)", client, err)
	}
}

func TestNewAPIClientDedicatedRemoteTLSTokenEnvBeatsVerifierTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("scoped-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", tokenPath)
	t.Setenv(serverTargetEnv, "https://board.example:8443")
	t.Setenv(serverTokenEnv, "remote-token")
	cfg := &config.Config{}
	client, err := newAPIClient(cfg)
	if err != nil || client == nil {
		t.Fatalf("dedicated remote TLS client rejected explicit token env: client=%+v err=%v", client, err)
	}
	if client.token != "remote-token" {
		t.Fatalf("token = %q, want explicit remote token", client.token)
	}
	if client.sandboxed {
		t.Fatal("explicit remote token should not be marked as sandbox-file sourced")
	}
}

// TestCLI_NeverTouchesTheBoardsFiles replaces a test that proved HTTP mode
// still worked when the local task dir was read-only. With no filesystem path
// left the interesting claim is stronger: a full create/update/list cycle works
// against a board while this machine's own task directory stays unwritable, so
// nothing in the CLI is reaching for it.
func TestCLI_NeverTouchesTheBoardsFiles(t *testing.T) {
	home := t.TempDir()
	tasksDir := useDefaultHTTPCLIHome(t, home)
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}

	serverTasksDir := t.TempDir()
	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	lockdownDir(t, tasksDir)

	code, out := runCLI(t, "--json", "create", "--title", "board only")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	served, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := served.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("board holds %v (err=%v), want the created task", list, err)
	}
	id := list[0].ID

	code, out = runCLI(t, "--json", "update", id, "--status", "todo", "--status-reason", "via the board")
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}
	got, err := served.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusTodo || got.StatusReason != "via the board" {
		t.Fatalf("board task = %+v, want the update to have landed there", got)
	}

	// The unwritable local directory is still empty: nothing wrote beside it.
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("read local task dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("local task dir holds %d entries; the CLI wrote board files", len(entries))
	}
}

func TestUpdate_UsesHTTPModeWhenTaskOnlyExistsOnServer(t *testing.T) {
	home := t.TempDir()
	_ = useDefaultHTTPCLIHome(t, home)

	serverTasksDir := t.TempDir()
	serverStore, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	serverTasks := task.NewManager(serverStore, nil)
	created, err := serverTasks.Create("api only target", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	code, out := runCLI(t, "--json", "update", created.ID, "--status", "todo", "--status-reason", "via http only")
	if code != 0 {
		t.Fatalf("update over HTTP mode should not require a local task file, got exit %d: %s", code, out)
	}
	var got task.Task
	mustUnmarshal(t, out, &got)
	if got.ID != created.ID || got.Status != task.StatusTodo || got.StatusReason != "via http only" {
		t.Fatalf("updated task = %+v, want server task updated through HTTP", got)
	}
}

func TestLinkPR_UsesHTTPModeWhenTaskOnlyExistsOnServer(t *testing.T) {
	home := t.TempDir()
	_ = useDefaultHTTPCLIHome(t, home)

	serverTasksDir := t.TempDir()
	serverStore, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	serverTasks := task.NewManager(serverStore, nil)
	created, err := serverTasks.Create("api only pr target", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	code, out := runCLI(t, "--json", "link-pr", created.ID, "123")
	if code != 0 {
		t.Fatalf("link-pr over HTTP mode should not require a local task file, got exit %d: %s", code, out)
	}
	var got task.Task
	mustUnmarshal(t, out, &got)
	if got.ID != created.ID || got.PRNumber != 123 || got.Status != task.StatusInReview {
		t.Fatalf("linked task = %+v, want server task updated through HTTP", got)
	}
}

// TestUpdate_FailsClosedWithNoServer keeps the refusal on the write path
// specifically: a read is obviously impossible without a board, but an update
// used to be the case that silently landed in this machine's files.
func TestUpdate_FailsClosedWithNoServer(t *testing.T) {
	home := t.TempDir()
	tasksDir := useDefaultHTTPCLIHome(t, home)
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeClosedPortFile(t, home)

	code, _, stderr := runCLIWithStderr(t, "--json", "update", "task-anything", "--status", "todo")
	if code == 0 {
		t.Fatal("update exit 0 with no server reachable")
	}
	if !strings.Contains(stderr, "no Sybra server is reachable") {
		t.Errorf("stderr = %q, want it to name the unreachable server", stderr)
	}
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("read local task dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("local task dir holds %d entries; the refused update wrote files", len(entries))
	}
}

// TestUpdate_ServerErrorNeverFallsBackToFilesystem keeps a 5xx a failure. The
// dangerous outcome is not the error but a silent local write behind it, which
// leaves this machine's files disagreeing with the board that refused.
func TestUpdate_ServerErrorNeverFallsBackToFilesystem(t *testing.T) {
	home := t.TempDir()
	tasksDir := useDefaultHTTPCLIHome(t, home)
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}

	port := startFailingAPIServer(t, t.TempDir())
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	code, out := runCLI(t, "--json", "update", "task-anything", "--status", "todo")
	if code == 0 {
		t.Fatalf("update must surface a real server error, not silently fall back to filesystem: exit 0, out=%s", out)
	}

	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("read local task dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("local task dir holds %d entries; a server error fell back to a direct write", len(entries))
	}
}

// TestHomeFlag_SelectsTheBoardNotTheFiles replaces a test asserting --home
// forced filesystem mode. A home now names which board's config and recorded
// port to read, so an explicit target still wins over it — the operator said
// where to go.
func TestHomeFlag_SelectsTheBoardNotTheFiles(t *testing.T) {
	home := t.TempDir()
	tasksDir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	isolateHTTPCLITestHome(t, home)

	serverTasksDir := t.TempDir()
	port := startFakeAPIServer(t, serverTasksDir)
	t.Setenv(serverTargetEnv, "127.0.0.1:"+port)

	lockdownDir(t, tasksDir)

	code, out := runCLI(t, "--json", "--home", home, "create", "--title", "home flag target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	served, err := task.NewStore(serverTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := served.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("board holds %v (err=%v), want the created task", list, err)
	}
}

// TestNewAPIClient_FallsBackToThisMachinesBoard replaces a pair of tests that
// asserted an unset target yields no client at all. That answer only made sense
// while "no client" meant "edit the files instead"; with no filesystem path
// left it would mean every command fails on a machine whose own board is up.
// TestLocalBoardCandidates_OrdersThisMachinesBoards replaces a pair of tests
// asserting an unset target yields no client at all. That answer only made
// sense while "no client" meant "edit the files instead".
//
// It is a list, not one answer: the desktop app's recorded port is kept across
// restarts on purpose, so a stale entry must not shadow a server that is up.
func TestLocalBoardCandidates_OrdersThisMachinesBoards(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv(serverTargetEnv, "")
	portFile := filepath.Join(home, desktopPortFile)

	t.Run("recorded desktop port is tried before the configured one", func(t *testing.T) {
		if err := os.WriteFile(portFile, []byte("51234\n"), 0o600); err != nil {
			t.Fatalf("write desktop port: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(portFile) })
		got := localBoardCandidates(config.DefaultConfig())
		want := []string{"127.0.0.1:51234", "127.0.0.1:" + config.DefaultServerPort}
		if !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})

	t.Run("configured port alone when nothing was recorded", func(t *testing.T) {
		got := localBoardCandidates(config.DefaultConfig())
		want := []string{"127.0.0.1:" + config.DefaultServerPort}
		if !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})

	t.Run("a corrupt port file is ignored rather than dialled", func(t *testing.T) {
		if err := os.WriteFile(portFile, []byte("not-a-port"), 0o600); err != nil {
			t.Fatalf("write desktop port: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(portFile) })
		got := localBoardCandidates(config.DefaultConfig())
		want := []string{"127.0.0.1:" + config.DefaultServerPort}
		if !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})

	t.Run("a bind locked to one interface is dialled there, not on loopback", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Cluster.BindAddrs = []string{"100.64.1.5:8080"}
		got := localBoardCandidates(cfg)
		if !slices.Equal(got, []string{"100.64.1.5:8080"}) {
			t.Fatalf("candidates = %v, want the configured bind; nothing listens on loopback there", got)
		}
	})

	t.Run("a TLS control plane is addressed over https", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Cluster.TLS.CertFile = "/tmp/cert.pem"
		cfg.Cluster.TLS.KeyFile = "/tmp/key.pem"
		got := localBoardCandidates(cfg)
		if len(got) != 1 || !strings.HasPrefix(got[0], "https://") {
			t.Fatalf("candidates = %v, want an https target; a TLS board refuses a cleartext hop", got)
		}
	})

	t.Run("ambient SYBRA_HOST and SYBRA_PORT do not steer the dial", func(t *testing.T) {
		// An unrelated unit shell exporting these must not aim the CLI — and
		// the bearer token it sends next — at whatever answers there.
		t.Setenv("SYBRA_HOST", "127.0.0.1")
		t.Setenv("SYBRA_PORT", "9999")
		got := localBoardCandidates(config.DefaultConfig())
		want := []string{"127.0.0.1:" + config.DefaultServerPort}
		if !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	})
}

func TestNewAPIClient_UsesDedicatedServerTargetEnv(t *testing.T) {
	t.Setenv(serverTargetEnv, "127.0.0.1:4123")
	cfg := config.DefaultConfig()
	cfg.Server.AuthToken = "token"

	client, err := newAPIClient(cfg)
	if err != nil || client == nil {
		t.Fatalf("newAPIClient() did not build a client from a valid dedicated target: %v", err)
	}
	if client.baseURL != "http://127.0.0.1:4123" {
		t.Fatalf("baseURL = %q, want http://127.0.0.1:4123", client.baseURL)
	}
}

func TestNewAPIClient_RejectsInvalidDedicatedServerTarget(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		// "empty" is absent on purpose: an unset target resolves to this
		// machine's board rather than being rejected.
		{name: "missing-host", raw: "8080"},
		{name: "blank-host", raw: ":8080"},
		{name: "wildcard-host", raw: "0.0.0.0:8080"},
		{name: "url-missing-port", raw: "http://127.0.0.1"},
		{name: "url-with-path", raw: "http://127.0.0.1:8080/api"},
		// A cleartext hop to another machine would put the bearer token on
		// the wire, and https without its token used to read as "unset".
		{name: "cleartext-to-another-machine", raw: "http://192.0.2.10:8080"},
		{name: "cleartext-hostname-to-another-machine", raw: "board.example:8080"},
		{name: "https-without-token", raw: "https://board.example:8443"},
		{name: "https-with-path", raw: "https://board.example:8443/api"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(serverTargetEnv, tc.raw)
			t.Setenv(serverTokenEnv, "")
			t.Setenv("SYBRA_AUTH_TOKEN_FILE", "")
			cfg := config.DefaultConfig()
			cfg.Server.AuthToken = "token"

			client, err := newAPIClient(cfg)
			if client != nil {
				t.Fatalf("newAPIClient() built a client for %q: %#v", tc.raw, client)
			}
			// An empty target is genuinely unset; every other case here is a
			// target the operator set and meant, so silence would send them
			// to this machine's files believing they had reached a board.
			if tc.raw == "" {
				if !errors.Is(err, errNoServerTarget) {
					t.Fatalf("unset target reported %v, want the unset sentinel", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("newAPIClient() silently ignored the configured target %q", tc.raw)
			}
		})
	}
}
