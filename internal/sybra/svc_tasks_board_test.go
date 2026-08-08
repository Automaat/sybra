package sybra

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/workflow"
)

// postAPI drives one call through the real handler, so what the test reads is
// what a client reads — including the sanitizing the handler applies to any
// error the service did not mark as a refusal.
func postAPI(t *testing.T, a *App, service, method string, args ...any) (status int, body string) {
	t.Helper()
	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.New(slog.DiscardHandler), nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	payload, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	resp, err := http.Post(srv.URL+"/api/"+service+"/"+method, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("POST %s/%s: %v", service, method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// TestBoardEndpoints_RejectionCarriesItsReason is deliberately end-to-end. A
// test that stubs the reply proves only that the client parses an envelope; it
// passes just as happily when every one of these refusals reaches the operator
// as "internal error", which is what they did.
func TestBoardEndpoints_RejectionCarriesItsReason(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	a.taskSvc = svc

	live, err := svc.tasks.Create("live", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.tasks.Delete(live.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.tasks.RestoreFromTrash(live.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := svc.tasks.Delete(live.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	revived, err := svc.tasks.RestoreFromTrash(live.ID)
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}

	tests := []struct {
		name       string
		service    string
		method     string
		args       []any
		wantStatus int
		wantReason string
	}{
		{
			name:       "restore onto a live id",
			service:    "TaskService",
			method:     "RestoreFromTrash",
			args:       []any{revived.ID},
			wantStatus: http.StatusConflict,
			wantReason: "refusing to overwrite with trashed copy",
		},
		{
			name:       "contradictory update",
			service:    "TaskService",
			method:     "UpdateTaskFields",
			args:       []any{revived.ID, task.Update{StatusReason: task.Ptr("why"), ClearStatusReason: task.Ptr(true)}},
			wantStatus: http.StatusBadRequest,
			wantReason: "cannot both be set",
		},
		{
			name:       "unknown status on create",
			service:    "TaskService",
			method:     "CreateTaskFull",
			args:       []any{"t", "", "headless", "not-a-status", task.Update{}},
			wantStatus: http.StatusBadRequest,
			wantReason: "not-a-status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := postAPI(t, a, tt.service, tt.method, tt.args...)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", status, tt.wantStatus, body)
			}
			if strings.Contains(body, "internal error") {
				t.Errorf("body %s sanitized the reason away", body)
			}
			if !strings.Contains(body, tt.wantReason) {
				t.Errorf("body %s does not carry %q", body, tt.wantReason)
			}
		})
	}
}

// TestCreateProjectAndClone_RejectionCarriesItsReason covers the other half of
// the board surface. A clone failure stays a 500 on purpose — it wraps the git
// invocation, which carries the server's clone path — but a URL the parser
// refuses is the operator's own input and has to come back readable.
func TestCreateProjectAndClone_RejectionCarriesItsReason(t *testing.T) {
	t.Parallel()
	store, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	a := &App{projectSvc: &ProjectService{projects: store, logger: slog.New(slog.DiscardHandler)}}

	status, body := postAPI(t, a, "ProjectService", "CreateProjectAndClone", "https://gitlab.com/acme/widget", "pet")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body %s)", status, http.StatusBadRequest, body)
	}
	if strings.Contains(body, "internal error") {
		t.Errorf("body %s sanitized the reason away", body)
	}
	if !strings.Contains(body, "unsupported host") {
		t.Errorf("body %s does not carry the parser's reason", body)
	}
}

// TestClassifyTask_RefusesANonFreshTaskWithItsReason pins the guard to the
// server. A client checking status against its own board would be checking one
// board and mutating another.
func TestClassifyTask_RefusesANonFreshTaskWithItsReason(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	svc.projects = projects
	a.taskSvc = svc

	created, createErr := svc.tasks.Create("already triaged", "", "headless")
	if createErr != nil {
		t.Fatalf("create: %v", createErr)
	}
	if _, err := svc.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusTodo)}); err != nil {
		t.Fatalf("update status: %v", err)
	}

	status, body := postAPI(t, a, "TaskService", "ClassifyTask", created.ID, "")
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d (body %s)", status, http.StatusConflict, body)
	}
	if !strings.Contains(body, "only reclassifies fresh tasks") {
		t.Errorf("body %s does not carry the guard's reason", body)
	}
}

// TestGetProjectRawType_KeepsUnsetDistinctFromPet guards the confidentiality
// routing. GetProject coerces an absent type to pet on the way out, so a
// client deriving the raw type from it would read a work project with no type
// field as pet and route it to an untrusted follower.
func TestGetProjectRawType_KeepsUnsetDistinctFromPet(t *testing.T) {
	t.Parallel()
	projectsDir := t.TempDir()
	store, err := project.NewStore(projectsDir, t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	record := "id: acme/widget\nname: widget\nowner: acme\nrepo: widget\nurl: https://github.com/acme/widget\n"
	if err := os.WriteFile(filepath.Join(projectsDir, "acme--widget.yaml"), []byte(record), 0o644); err != nil {
		t.Fatalf("write project record: %v", err)
	}
	svc := &ProjectService{projects: store, logger: slog.New(slog.DiscardHandler)}

	coerced, err := svc.GetProject("acme/widget")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if coerced.Type != project.ProjectTypePet {
		t.Fatalf("GetProject type = %q; the coercion this test guards against is gone, revisit RawType", coerced.Type)
	}

	raw, err := svc.GetProjectRawType("acme/widget")
	if err != nil {
		t.Fatalf("GetProjectRawType: %v", err)
	}
	if raw != "" {
		t.Errorf("GetProjectRawType = %q, want the unset value", raw)
	}
}

// TestAppendTaskProgress_WritesBesideTheBoard covers the split a CLI append
// otherwise has: the entry lands in the client's own artifact dir while the
// task it describes lives on another machine.
func TestAppendTaskProgress_WritesBesideTheBoard(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())

	created, err := svc.tasks.Create("progress", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	entry, err := svc.AppendTaskProgress(created.ID, artifact.ProgressKindProgress, "implementation", "picked the reducer")
	if err != nil {
		t.Fatalf("AppendTaskProgress: %v", err)
	}
	if entry.Message != "picked the reducer" {
		t.Errorf("entry.Message = %q", entry.Message)
	}

	entries, err := svc.ListTaskProgress(created.ID)
	if err != nil {
		t.Fatalf("ListTaskProgress: %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "picked the reducer" {
		t.Fatalf("entries = %+v, want the one appended entry", entries)
	}

	after, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("UpdatedAt %v not bumped past %v; an out-of-band append stays invisible", after.UpdatedAt, before.UpdatedAt)
	}

	if _, err := svc.AppendTaskProgress(created.ID, "not-a-kind", "", "x"); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := svc.AppendTaskProgress(created.ID, artifact.ProgressKindProgress, "", "   "); err == nil {
		t.Error("an empty message was accepted")
	}
}

// TestExpandUmbrella_ForwardsTheRequestedModel keeps `sybra-cli umbrella
// --model` meaningful against a server. Dropping the flag would expand with
// the instance default and still report success, so the operator sees no sign
// the model they asked for was ignored.
func TestExpandUmbrella_ForwardsTheRequestedModel(t *testing.T) {
	t.Parallel()
	var got string
	svc := &TaskService{umbrellaExpand: func(_, model string) (umbrella.Result, error) {
		got = model
		return umbrella.Result{Created: 1}, nil
	}}

	if _, err := svc.ExpandUmbrella("https://github.com/acme/widget/issues/1", "claude-opus-5"); err != nil {
		t.Fatalf("ExpandUmbrella: %v", err)
	}
	if got != "claude-opus-5" {
		t.Errorf("model = %q, want the requested one", got)
	}

	if _, err := svc.ExpandUmbrella("https://github.com/acme/widget/issues/1", ""); err != nil {
		t.Fatalf("ExpandUmbrella: %v", err)
	}
	if got != "" {
		t.Errorf("model = %q, want the empty value that lets the instance choose", got)
	}
}

// TestUpdateTaskFields_ClearWorkflowSurvivesTheWire is the reason
// Update.ClearWorkflow exists. Workflow is a **Execution: the old encoding of
// a clear was a non-nil outer pointer holding a nil inner one, which marshals
// to null and unmarshals back to "leave unchanged", so `sybra-cli reopen`
// against a server left the dead execution attached and the task wedged.
func TestUpdateTaskFields_ClearWorkflowSurvivesTheWire(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	a.taskSvc = svc

	created, err := svc.tasks.Create("wedged", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.tasks.Update(created.ID, task.Update{
		Workflow: task.Ptr(&workflow.Execution{WorkflowID: "simple-task-implement", State: workflow.ExecRunning}),
	}); err != nil {
		t.Fatalf("attach workflow: %v", err)
	}

	status, body := postAPI(t, a, "TaskService", "UpdateTaskFields", created.ID, task.Update{ClearWorkflow: task.Ptr(true)})
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, body)
	}

	after, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Workflow != nil {
		t.Errorf("Workflow = %+v after a clear sent over the wire, want nil", after.Workflow)
	}
}

// TestBoardEndpoints_NotFoundNamesTheIdentifier covers what an operator does
// most often with a bad argument: mistype an id. The stores answer a miss with
// an error naming the absolute path they looked at, so the handler flattens it
// to a bare "not found" — which leaves the operator unable to tell which
// argument was wrong. The identifier has to survive; the path must not.
func TestBoardEndpoints_NotFoundNamesTheIdentifier(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	a.taskSvc = svc
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	a.projectSvc = &ProjectService{projects: projects, logger: slog.New(slog.DiscardHandler)}

	tests := []struct {
		name    string
		service string
		method  string
		args    []any
		wantID  string
	}{
		{"get a missing task", "TaskService", "GetTask", []any{"zzzzzzzz"}, "zzzzzzzz"},
		{"delete a missing task", "TaskService", "DeleteTask", []any{"zzzzzzzz"}, "zzzzzzzz"},
		{"append progress to a missing task", "TaskService", "AppendTaskProgress", []any{"zzzzzzzz", "progress", "", "hi"}, "zzzzzzzz"},
		{"update a missing task", "TaskService", "UpdateTaskFields", []any{"zzzzzzzz", task.Update{Title: task.Ptr("x")}}, "zzzzzzzz"},
		{"read a missing artifact", "TaskService", "ReadTaskArtifact", []any{"zzzzzzzz", "nope.txt"}, "nope.txt"},
		{"get an unregistered project", "ProjectService", "GetProject", []any{"owner/nope"}, "owner/nope"},
		{"raw type of an unregistered project", "ProjectService", "GetProjectRawType", []any{"owner/nope"}, "owner/nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := postAPI(t, a, tt.service, tt.method, tt.args...)
			if status != http.StatusNotFound {
				t.Errorf("status = %d, want %d (body %s)", status, http.StatusNotFound, body)
			}
			if !strings.Contains(body, tt.wantID) {
				t.Errorf("body %s does not name %q", body, tt.wantID)
			}
			for _, leak := range []string{"/var/folders", "/tmp/", ".md", ".yaml"} {
				if strings.Contains(body, leak) {
					t.Errorf("body %s leaks the server's filesystem layout via %q", body, leak)
				}
			}
		})
	}
}

// TestUpdateTask_InvalidStatusCarriesTheValidSet keeps the most common
// mistyped argument actionable. The filesystem-backed form lists every valid
// status, and a bare "internal error" would send the operator to the source.
func TestUpdateTask_InvalidStatusCarriesTheValidSet(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	a.taskSvc = svc

	created, err := svc.tasks.Create("bad status target", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	status, body := postAPI(t, a, "TaskService", "UpdateTask", created.ID, map[string]any{"status": "bogus"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (body %s)", status, http.StatusBadRequest, body)
	}
	if !strings.Contains(body, "bogus") || !strings.Contains(body, "in-progress") {
		t.Errorf("body %s does not name the rejected value and the valid set", body)
	}
}

// TestBoardEndpoints_InputRejectionsCarryTheirReason locks the whole class
// rather than the handful of paths a tester happened to try.
//
// Two adversarial passes found this same defect at call sites the previous
// fix had not enumerated, which is the argument for marking the validations
// where they are raised instead of where they are returned. Every case below
// is a value the caller supplied, so every one has to come back readable.
func TestBoardEndpoints_InputRejectionsCarryTheirReason(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	a.taskSvc = svc
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	if _, err := projects.Create("https://github.com/acme/widget", project.ProjectTypePet); err != nil {
		t.Skipf("project create needs git: %v", err)
	}
	a.projectSvc = &ProjectService{projects: projects, logger: slog.New(slog.DiscardHandler)}

	created, err := svc.tasks.Create("rejection target", "", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	tests := []struct {
		name       string
		service    string
		method     string
		args       []any
		wantReason string
	}{
		{"create with an unknown mode", "TaskService", "CreateTask", []any{"t", "", "bogusmode"}, "bogusmode"},
		{"update to an unknown mode", "TaskService", "UpdateTask", []any{created.ID, map[string]any{"agent_mode": "bogus"}}, "bogus"},
		{"update an unknown field", "TaskService", "UpdateTask", []any{created.ID, map[string]any{"nope": "x"}}, "nope"},
		{"project type that is neither", "ProjectService", "UpdateProject", []any{"acme/widget", "bogus"}, "pet or work"},
		{"artifact name escaping the store", "TaskService", "ReadTaskArtifact", []any{created.ID, "../../etc/passwd"}, "invalid artifact name"},
		{"task id escaping the store", "TaskService", "GetTask", []any{"../../etc/passwd"}, "invalid task ID"},
		{"task id escaping the artifact store", "TaskService", "ListTaskArtifactMetas", []any{"../../etc/passwd"}, "invalid task id"},
		{"delete a task id escaping the store", "TaskService", "DeleteTask", []any{"../../etc/passwd"}, "invalid task ID"},
		{"progress against an escaping task id", "TaskService", "AppendTaskProgress", []any{"../../etc/passwd", "progress", "", "hi"}, "invalid task ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := postAPI(t, a, tt.service, tt.method, tt.args...)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body %s)", status, http.StatusBadRequest, body)
			}
			if strings.Contains(body, "internal error") {
				t.Errorf("body %s sanitized the reason away", body)
			}
			if !strings.Contains(body, tt.wantReason) {
				t.Errorf("body %s does not carry %q", body, tt.wantReason)
			}
		})
	}
}

// TestBoardEndpoints_EnvelopeCodeMatchesTheStatus keeps the machine-readable
// half of a rejection honest. A client branching on code rather than status
// saw "validation_error" on a 404, which reads as "your input was malformed"
// for a well-formed id that simply is not there.
func TestBoardEndpoints_EnvelopeCodeMatchesTheStatus(t *testing.T) {
	t.Parallel()
	svc, a := setupTaskService(t)
	a.taskSvc = svc

	_, body := postAPI(t, a, "TaskService", "GetTask", "zzzzzzzz")
	if !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("body %s does not carry the not_found code", body)
	}
	if strings.Contains(body, "validation_error") {
		t.Errorf("body %s reports a missing id as malformed input", body)
	}
}
