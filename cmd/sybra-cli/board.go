package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// taskBoard and projectBoard are the board surface every CLI command uses.
//
// The method set is deliberately identical to *task.Manager's and
// *project.Store's, so those satisfy it with no adapter and no call site
// changes. The point of the seam is the other implementation: apiTaskBoard
// sends each operation to a running server, so a command run on one machine
// against a task owned by another changes that board instead of editing a
// local file the owning instance never reads.
type taskBoard interface {
	List() ([]task.Task, error)
	Get(id string) (task.Task, error)
	Create(title, body, mode string) (task.Task, error)
	CreateFull(title, body, mode string, init task.Update) (task.Task, error)
	CreateWithStatus(title, body, mode string, status task.Status, extra task.Update) (task.Task, error)
	Update(id string, u task.Update) (task.Task, error)
	UpdateMap(id string, raw map[string]any) (task.Task, error)
	Apply(intent task.TransitionIntent) (task.TransitionResult, error)
	Touch(id string) (task.Task, error)
	Delete(id string) error
	ListTrash() ([]task.TrashEntry, error)
	RestoreFromTrash(id string) (task.Task, error)
	DeleteTrashedGeneration(id string) (bool, error)
	PruneAllTrash() (task.TrashPruneReport, error)
}

type projectBoard interface {
	List() ([]project.Project, error)
	Get(id string) (project.Project, error)
	RawType(id string) (project.ProjectType, error)
	Create(rawURL string, ptype project.ProjectType) (project.Project, error)
	Update(id string, ptype project.ProjectType) (project.Project, error)
	SetSetupCommands(id string, cmds []string) (project.Project, error)
	Delete(id string) error
}

var (
	_ taskBoard    = (*task.Manager)(nil)
	_ projectBoard = (*project.Store)(nil)
)

const taskServiceName = "TaskService"

const projectServiceName = "ProjectService"

// apiTaskBoard runs every board operation against a reachable server.
type apiTaskBoard struct {
	api *apiClient
}

func newAPITaskBoard(api *apiClient) *apiTaskBoard { return &apiTaskBoard{api: api} }

func (b *apiTaskBoard) List() ([]task.Task, error) {
	return callAPI[[]task.Task](b.api, taskServiceName, "ListTasks")
}

func (b *apiTaskBoard) Get(id string) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "GetTask", id)
}

func (b *apiTaskBoard) Create(title, body, mode string) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "CreateTask", title, body, mode)
}

func (b *apiTaskBoard) CreateFull(title, body, mode string, init task.Update) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "CreateTaskFull", title, body, mode, "", init)
}

func (b *apiTaskBoard) CreateWithStatus(title, body, mode string, status task.Status, extra task.Update) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "CreateTaskFull", title, body, mode, string(status), extra)
}

func (b *apiTaskBoard) Update(id string, u task.Update) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "UpdateTaskFields", id, u)
}

func (b *apiTaskBoard) UpdateMap(id string, raw map[string]any) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "UpdateTask", id, raw)
}

func (b *apiTaskBoard) Apply(intent task.TransitionIntent) (task.TransitionResult, error) {
	return callAPI[task.TransitionResult](b.api, taskServiceName, "ApplyTransition", intent)
}

func (b *apiTaskBoard) Touch(id string) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "TouchTask", id)
}

func (b *apiTaskBoard) Delete(id string) error {
	_, err := callAPI[struct{}](b.api, taskServiceName, "DeleteTask", id)
	return err
}

func (b *apiTaskBoard) ListTrash() ([]task.TrashEntry, error) {
	return callAPI[[]task.TrashEntry](b.api, taskServiceName, "ListTrash")
}

func (b *apiTaskBoard) RestoreFromTrash(id string) (task.Task, error) {
	return callAPI[task.Task](b.api, taskServiceName, "RestoreFromTrash", id)
}

func (b *apiTaskBoard) DeleteTrashedGeneration(id string) (bool, error) {
	return callAPI[bool](b.api, taskServiceName, "DeleteTrashedGeneration", id)
}

func (b *apiTaskBoard) PruneAllTrash() (task.TrashPruneReport, error) {
	dto, err := callAPI[trashPruneReportDTO](b.api, taskServiceName, "PruneAllTrash")
	if err != nil {
		return task.TrashPruneReport{}, err
	}
	report := task.TrashPruneReport{Scanned: dto.Scanned, Removed: dto.Removed, Entries: dto.Entries}
	for _, message := range dto.Errors {
		report.Errors = append(report.Errors, fmt.Errorf("%s", message))
	}
	return report, nil
}

// trashPruneReportDTO mirrors the server's wire shape for a prune report. The domain type carries []error, which JSON renders as empty objects.
type trashPruneReportDTO struct {
	Scanned int               `json:"scanned"`
	Removed int               `json:"removed"`
	Entries []task.TrashEntry `json:"entries"`
	Errors  []string          `json:"errors,omitempty"`
}

// apiProjectBoard reads project metadata from a reachable server.
type apiProjectBoard struct {
	api *apiClient
}

func newAPIProjectBoard(api *apiClient) *apiProjectBoard { return &apiProjectBoard{api: api} }

func (b *apiProjectBoard) List() ([]project.Project, error) {
	return callAPI[[]project.Project](b.api, projectServiceName, "ListProjects")
}

func (b *apiProjectBoard) Get(id string) (project.Project, error) {
	return callAPI[project.Project](b.api, projectServiceName, "GetProject", id)
}

// RawType reads the stored type without the store's normalization, so a
// caller can tell "unset" from "pet". It needs its own endpoint: GetProject
// coerces an absent type to pet on the way out, which would make a work
// project with no type field read as pet and route it to an untrusted
// follower.
func (b *apiProjectBoard) RawType(id string) (project.ProjectType, error) {
	raw, err := callAPI[string](b.api, projectServiceName, "GetProjectRawType", id)
	if err != nil {
		return "", err
	}
	return project.ProjectType(raw), nil
}

// Create waits for the clone. CreateProject returns as soon as the record is
// registered, which suits the GUI but would have the CLI exit 0 on a repo that
// never cloned, printing a record in `cloning` where the filesystem-backed
// command printed `ready`.
func (b *apiProjectBoard) Create(rawURL string, ptype project.ProjectType) (project.Project, error) {
	return callAPIWithin[project.Project](b.api, apiCloneTimeout, projectServiceName, "CreateProjectAndClone", rawURL, string(ptype))
}

func (b *apiProjectBoard) Update(id string, ptype project.ProjectType) (project.Project, error) {
	return callAPI[project.Project](b.api, projectServiceName, "UpdateProject", id, string(ptype))
}

func (b *apiProjectBoard) SetSetupCommands(id string, cmds []string) (project.Project, error) {
	return callAPI[project.Project](b.api, projectServiceName, "SetProjectSetupCommands", id, cmds)
}

func (b *apiProjectBoard) Delete(id string) error {
	_, err := callAPI[struct{}](b.api, projectServiceName, "DeleteProject", id)
	return err
}

// callAPI reports the server's own reason for a rejection rather than a generic failure, so an operator sees the same message the GUI would.
func callAPI[T any](api *apiClient, service, method string, args ...any) (T, error) {
	return callAPIWithin[T](api, apiCallTimeout, service, method, args...)
}

// callAPIWithin is callAPI with an explicit budget, for the endpoints that run a model on the server.
func callAPIWithin[T any](api *apiClient, timeout time.Duration, service, method string, args ...any) (T, error) {
	var out T
	if api == nil {
		return out, fmt.Errorf("no server target configured")
	}
	if err := api.callWithin(context.Background(), timeout, service, method, &out, args...); err != nil {
		return out, err
	}
	return out, nil
}
