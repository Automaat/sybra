package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// The CLI has no second way to reach a board, so a test that exercises a board
// command needs a server. This is that server: the real stores, mounted on the
// real HTTP dispatcher, under the method names the CLI calls.
//
// It is deliberately not a stub of the responses. Mounting the stores means a
// test still asserts against the files on disk, and an error still travels the
// path the handler maps — a missing task is a 404 here exactly as it is against
// sybra-server, rather than whatever a hand-written stub decided to return.

// testBoardTaskService adapts task.Manager to the wire names TaskService uses.
type testBoardTaskService struct {
	tasks     *task.Manager
	artifacts *artifact.Store
}

func (s *testBoardTaskService) ListTasks() ([]task.Task, error) { return s.tasks.List() }
func (s *testBoardTaskService) GetTask(id string) (task.Task, error) {
	return s.tasks.Get(id)
}

func (s *testBoardTaskService) CreateTask(title, body, mode string) (task.Task, error) {
	return s.tasks.Create(title, body, mode)
}

func (s *testBoardTaskService) CreateTaskFull(title, body, mode, status string, init task.Update) (task.Task, error) {
	if status == "" {
		return s.tasks.CreateFull(title, body, mode, init)
	}
	st, err := task.ValidateStatus(status)
	if err != nil {
		return task.Task{}, err
	}
	return s.tasks.CreateWithStatus(title, body, mode, st, init)
}

func (s *testBoardTaskService) UpdateTaskFields(id string, u task.Update) (task.Task, error) {
	before, err := s.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	after, err := s.tasks.Update(id, u)
	if err != nil {
		return task.Task{}, err
	}
	s.recordManualDecision(before, after)
	return after, nil
}

func (s *testBoardTaskService) UpdateTask(id string, raw map[string]any) (task.Task, error) {
	before, err := s.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	after, err := s.tasks.UpdateMap(id, raw)
	if err != nil {
		return task.Task{}, err
	}
	s.recordManualDecision(before, after)
	return after, nil
}

// recordManualDecision mirrors TaskService.appendManualHumanRequiredDecision.
// The CLI stopped writing this entry itself once every update went through a
// server, so a board that does not record it would let that removal look
// harmless while the decision log silently stopped being written.
func (s *testBoardTaskService) recordManualDecision(before, after task.Task) {
	if before.Status != task.StatusHumanRequired || after.Status == task.StatusHumanRequired {
		return
	}
	_ = s.artifacts.AppendProgress(after.ID, artifact.ProgressEntry{
		Ts:      time.Now().UTC(),
		Kind:    artifact.ProgressKindDecision,
		Message: artifact.ManualDecisionMessage(string(before.Status), string(after.Status), after.StatusReason),
	})
}

func (s *testBoardTaskService) ApplyTransition(intent task.TransitionIntent) (task.TransitionResult, error) {
	return s.tasks.Apply(intent)
}

func (s *testBoardTaskService) TouchTask(id string) (task.Task, error) { return s.tasks.Touch(id) }
func (s *testBoardTaskService) DeleteTask(id string) error             { return s.tasks.Delete(id) }
func (s *testBoardTaskService) ListTrash() ([]task.TrashEntry, error)  { return s.tasks.ListTrash() }
func (s *testBoardTaskService) RestoreFromTrash(id string) (task.Task, error) {
	return s.tasks.RestoreFromTrash(id)
}

func (s *testBoardTaskService) DeleteTrashedGeneration(id string) (bool, error) {
	return s.tasks.DeleteTrashedGeneration(id)
}

func (s *testBoardTaskService) PruneAllTrash() (trashPruneReportDTO, error) {
	report, err := s.tasks.PruneAllTrash()
	if err != nil {
		return trashPruneReportDTO{}, err
	}
	dto := trashPruneReportDTO{Scanned: report.Scanned, Removed: report.Removed, Entries: report.Entries}
	for _, e := range report.Errors {
		dto.Errors = append(dto.Errors, e.Error())
	}
	return dto, nil
}

// AppendTaskProgress mirrors TaskService's own validation and timestamping, so
// a CLI test sees the rejections and the stamped entry a real board returns.
func (s *testBoardTaskService) AppendTaskProgress(taskID, kind, role, message string) (artifact.ProgressEntry, error) {
	if !artifact.ValidProgressKind(kind) {
		return artifact.ProgressEntry{}, fmt.Errorf("invalid progress kind %s", kind)
	}
	if strings.TrimSpace(message) == "" {
		return artifact.ProgressEntry{}, errors.New("progress message is required")
	}
	if _, err := s.tasks.Get(taskID); err != nil {
		return artifact.ProgressEntry{}, err
	}
	entry := artifact.ProgressEntry{Ts: time.Now().UTC(), Kind: kind, Role: role, Message: message}
	if err := s.artifacts.AppendProgress(taskID, entry); err != nil {
		return artifact.ProgressEntry{}, err
	}
	if _, err := s.tasks.Touch(taskID); err != nil {
		return artifact.ProgressEntry{}, err
	}
	return entry, nil
}

func (s *testBoardTaskService) ListTaskProgress(taskID string) ([]artifact.ProgressEntry, error) {
	entries, err := s.artifacts.ReadProgress(taskID)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []artifact.ProgressEntry{}
	}
	return entries, nil
}

func (s *testBoardTaskService) ListTaskArtifactMetas(taskID string) ([]artifact.Meta, error) {
	return s.artifacts.List(taskID)
}

func (s *testBoardTaskService) ReadTaskArtifact(taskID, name string) ([]byte, error) {
	data, _, err := s.artifacts.Read(taskID, name)
	return data, err
}

func (s *testBoardTaskService) ReindexTaskArtifacts(taskID string) error {
	return s.artifacts.Reindex(taskID)
}

// testBoardProjectService adapts project.Store to ProjectService's wire names.
type testBoardProjectService struct{ projects *project.Store }

func (s *testBoardProjectService) ListProjects() ([]project.Project, error) {
	return s.projects.List()
}

func (s *testBoardProjectService) GetProject(id string) (project.Project, error) {
	return s.projects.Get(id)
}

func (s *testBoardProjectService) GetProjectRawType(id string) (string, error) {
	raw, err := s.projects.RawType(id)
	return string(raw), err
}

func (s *testBoardProjectService) CreateProjectAndClone(rawURL, ptype string) (project.Project, error) {
	return s.projects.Create(rawURL, project.ProjectType(ptype))
}

func (s *testBoardProjectService) UpdateProject(id, ptype string) (project.Project, error) {
	return s.projects.Update(id, project.ProjectType(ptype))
}

func (s *testBoardProjectService) SetProjectSetupCommands(id string, cmds []string) (project.Project, error) {
	return s.projects.SetSetupCommands(id, cmds)
}

func (s *testBoardProjectService) DeleteProject(id string) error { return s.projects.Delete(id) }

// startTestBoard serves home's stores over the API and points the CLI at it.
func startTestBoard(t *testing.T, home string) *httptest.Server {
	t.Helper()

	rawStore, err := task.NewStore(filepath.Join(home, "tasks"))
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	projects, err := project.NewStore(filepath.Join(home, "projects"), filepath.Join(home, "clones"))
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}

	mux := http.NewServeMux()
	// The reachability probe the CLI runs before it will use a target.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(&testBoardTaskService{
			tasks:     task.NewManager(rawStore, nil),
			artifacts: artifact.New(filepath.Join(home, "artifacts")),
		},
			"ListTasks", "GetTask", "CreateTask", "CreateTaskFull", "UpdateTaskFields",
			"UpdateTask", "ApplyTransition", "TouchTask", "DeleteTask", "ListTrash",
			"RestoreFromTrash", "DeleteTrashedGeneration", "PruneAllTrash",
			"AppendTaskProgress", "ListTaskProgress", "ListTaskArtifactMetas",
			"ReadTaskArtifact", "ReindexTaskArtifacts",
		),
		"ProjectService": httpapi.NewService(&testBoardProjectService{projects: projects},
			"ListProjects", "GetProject", "GetProjectRawType", "CreateProjectAndClone",
			"UpdateProject", "SetProjectSetupCommands", "DeleteProject",
		),
	}, slog.New(slog.DiscardHandler), nil)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test board URL: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split test board host: %v", err)
	}
	// Recorded the way the desktop app records it, rather than exported as
	// SYBRA_SERVER_TARGET: that is the discovery path an operator who set no
	// target actually takes, and it is per-home, so --home reaches this board
	// only for the home it belongs to.
	if err := os.WriteFile(filepath.Join(home, desktopPortFile), []byte(port), 0o600); err != nil {
		t.Fatalf("write desktop port: %v", err)
	}
	tokenPath := filepath.Join(home, "test-board-token")
	if err := os.WriteFile(tokenPath, []byte("test-board-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(serverTargetEnv, "")
	t.Setenv("SYBRA_AUTH_TOKEN_FILE", tokenPath)
	return srv
}

// newTestBoardClient starts a board over home and returns a client for it, for
// the tests that drive one command directly rather than through run().
func newTestBoardClient(t *testing.T, home string) *apiClient {
	t.Helper()
	srv := startTestBoard(t, home)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test board URL: %v", err)
	}
	return &apiClient{baseURL: "http://" + u.Host, token: "test-board-token", http: srv.Client()}
}
