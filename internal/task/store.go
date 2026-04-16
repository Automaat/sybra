package task

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/google/uuid"
)

type Store struct {
	dir           string
	comments      *CommentStore
	plans         *PlanStore
	planCritiques *PlanCritiqueStore
	codeReviews   *CodeReviewStore
	cacheMu       sync.RWMutex
	listCache     []Task
	listValid     bool
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}
	return &Store{
		dir:           dir,
		comments:      NewCommentStore(dir),
		plans:         NewPlanStore(dir),
		planCritiques: NewPlanCritiqueStore(dir),
		codeReviews:   NewCodeReviewStore(dir),
	}, nil
}

func (s *Store) Comments() *CommentStore {
	return s.comments
}

func (s *Store) Plans() *PlanStore {
	return s.plans
}

func (s *Store) PlanCritiques() *PlanCritiqueStore {
	return s.planCritiques
}

func (s *Store) CodeReviews() *CodeReviewStore {
	return s.codeReviews
}

func (s *Store) List() ([]Task, error) {
	if tasks, ok := s.cachedList(); ok {
		return tasks, nil
	}

	paths, err := fsutil.ListFiles(s.dir, ".md")
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}

	var tasks []Task
	var parseErr bool
	for _, p := range paths {
		base := filepath.Base(p)
		if strings.HasSuffix(base, ".plan.md") || strings.HasSuffix(base, ".plan-critique.md") || strings.HasSuffix(base, ".review.md") {
			continue
		}
		t, err := Parse(p)
		if err != nil {
			slog.Default().Warn("task.parse.skip", "file", filepath.Base(p), "err", err)
			parseErr = true
			continue
		}
		t.Plan, _ = s.plans.Read(t.ID)
		t.PlanCritique, _ = s.planCritiques.Read(t.ID)
		t.CodeReview, _ = s.codeReviews.Read(t.ID)
		// One-time migration: stamp ClosedAt for legacy terminal tasks that
		// predate the ClosedAt field. UpdatedAt is the best approximation.
		if IsTerminalStatus(t.Status) && t.ClosedAt == nil {
			ts := t.UpdatedAt
			t.ClosedAt = &ts
			if data, merr := Marshal(t); merr == nil {
				_ = fsutil.AtomicWrite(p, data)
			}
		}
		tasks = append(tasks, t)
	}
	if !parseErr {
		s.storeListCache(tasks)
	}
	return tasks, nil
}

func (s *Store) Get(id string) (Task, error) {
	path, err := s.safePath(id)
	if err != nil {
		return Task{}, err
	}
	t, err := Parse(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Task{}, fmt.Errorf("task %s not found", id)
		}
		return Task{}, err
	}
	t.Plan, _ = s.plans.Read(t.ID)
	t.PlanCritique, _ = s.planCritiques.Read(t.ID)
	t.CodeReview, _ = s.codeReviews.Read(t.ID)
	return t, nil
}

// safePath joins id to the store directory and confirms the resolved path
// stays inside it. Without this guard a CLI caller could pass `../../etc/x`
// and Get/Delete/Update would happily walk outside the tasks dir — agents
// routinely call sybra-cli with task IDs they parsed from prompts, so the
// untrusted-input surface is real even though the GUI generates IDs itself.
func (s *Store) safePath(id string) (string, error) {
	path := filepath.Clean(filepath.Join(s.dir, id+".md"))
	if !strings.HasPrefix(path, filepath.Clean(s.dir)+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid task ID %q", id)
	}
	return path, nil
}

func (s *Store) Create(title, body, mode string) (Task, error) {
	if mode == "" {
		mode = AgentModeInteractive
	}
	if _, err := ValidateAgentMode(mode); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	id := uuid.NewString()[:8]
	t := Task{
		ID:        id,
		Slug:      Slugify(title),
		Title:     title,
		Status:    StatusTodo,
		TaskType:  TaskTypeNormal,
		AgentMode: mode,
		CreatedAt: now,
		UpdatedAt: now,
		Body:      body,
	}

	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}

	filename := fmt.Sprintf("%s.md", t.ID)
	t.FilePath = filepath.Join(s.dir, filename)
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, fmt.Errorf("write task file: %w", err)
	}
	s.storeTaskCache(t)
	return t, nil
}

// CreateChat creates a synthetic chat task bound to projectID. Chat tasks are
// hidden from the task list UI and never restart on app reboot. The slug is
// "chat-<8char>" so the worktree DirName is distinctive.
func (s *Store) CreateChat(projectID string) (Task, error) {
	if projectID == "" {
		return Task{}, fmt.Errorf("project_id is required for chat")
	}
	now := time.Now().UTC()
	id := uuid.NewString()[:8]
	title := "chat " + now.Format("01-02 15:04")
	t := Task{
		ID:        id,
		Slug:      "chat-" + id,
		Title:     title,
		Status:    StatusInProgress,
		TaskType:  TaskTypeChat,
		AgentMode: AgentModeInteractive,
		ProjectID: projectID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}
	filename := fmt.Sprintf("%s.md", t.ID)
	t.FilePath = filepath.Join(s.dir, filename)
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, fmt.Errorf("write chat task file: %w", err)
	}
	s.storeTaskCache(t)
	return t, nil
}

func (s *Store) Delete(id string) error {
	t, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := os.Remove(t.FilePath); err != nil {
		return fmt.Errorf("delete task file: %w", err)
	}
	_ = s.comments.DeleteAll(id)
	_ = s.plans.Delete(id)
	_ = s.planCritiques.Delete(id)
	_ = s.codeReviews.Delete(id)
	s.deleteCachedTask(id)
	return nil
}

func (s *Store) Update(id string, u Update) (Task, error) {
	t, err := s.Get(id)
	if err != nil {
		return Task{}, err
	}

	if u.Title != nil {
		t.Title = *u.Title
	}
	if u.Slug != nil {
		t.Slug = *u.Slug
	}
	if u.Status != nil {
		oldStatus := t.Status
		t.Status = *u.Status
		// Clear reason when status changes unless a new reason is also provided.
		if u.StatusReason == nil {
			t.StatusReason = ""
		}
		// Stamp ClosedAt on transition into a terminal status; clear on exit.
		wasTerminal := IsTerminalStatus(oldStatus)
		isTerminal := IsTerminalStatus(t.Status)
		if !wasTerminal && isTerminal {
			now := time.Now().UTC()
			t.ClosedAt = &now
		} else if wasTerminal && !isTerminal {
			t.ClosedAt = nil
		}
		// both terminal → preserve existing ClosedAt; both non-terminal → no-op
	}
	if u.StatusReason != nil {
		t.StatusReason = *u.StatusReason
	}
	if u.AgentMode != nil {
		if _, err := ValidateAgentMode(*u.AgentMode); err != nil {
			return Task{}, err
		}
		t.AgentMode = *u.AgentMode
	}
	if u.TaskType != nil {
		t.TaskType = *u.TaskType
	}
	if u.Body != nil {
		t.Body = *u.Body
	}
	if u.Tags != nil {
		t.Tags = *u.Tags
	}
	if u.ProjectID != nil {
		t.ProjectID = *u.ProjectID
	}
	if u.Branch != nil {
		t.Branch = *u.Branch
	}
	if u.PRNumber != nil {
		t.PRNumber = *u.PRNumber
	}
	if u.Issue != nil {
		t.Issue = *u.Issue
	}
	if u.Reviewed != nil {
		t.Reviewed = *u.Reviewed
	}
	if u.RunRole != nil {
		t.RunRole = *u.RunRole
	}
	if u.TodoistID != nil {
		t.TodoistID = *u.TodoistID
	}
	if u.Priority != nil {
		t.Priority = *u.Priority
	}
	if u.DueDate != nil {
		t.DueDate = *u.DueDate
	}
	if u.Workflow != nil {
		t.Workflow = *u.Workflow
	}
	if u.Plan != nil {
		if wErr := s.plans.Write(id, *u.Plan); wErr != nil {
			return Task{}, fmt.Errorf("write plan: %w", wErr)
		}
		t.Plan = *u.Plan
	}
	if u.PlanCritique != nil {
		if wErr := s.planCritiques.Write(id, *u.PlanCritique); wErr != nil {
			return Task{}, fmt.Errorf("write plan critique: %w", wErr)
		}
		t.PlanCritique = *u.PlanCritique
	}
	if u.CodeReview != nil {
		if wErr := s.codeReviews.Write(id, *u.CodeReview); wErr != nil {
			return Task{}, fmt.Errorf("write code review: %w", wErr)
		}
		t.CodeReview = *u.CodeReview
	}

	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, fmt.Errorf("write task file: %w", err)
	}
	s.storeTaskCache(t)
	return t, nil
}

// InvalidatePath clears any cached task/list state for the given task file.
// Non-task files are ignored.
func (s *Store) InvalidatePath(path string) {
	if !strings.HasSuffix(path, ".md") {
		return
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".plan.md") || strings.HasSuffix(base, ".plan-critique.md") || strings.HasSuffix(base, ".review.md") {
		s.invalidateListCache()
		return
	}
	id := strings.TrimSuffix(base, ".md")
	if id == "" {
		return
	}
	s.invalidateListCache()
}

func (s *Store) cachedList() ([]Task, bool) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	if !s.listValid {
		return nil, false
	}
	return cloneTasks(s.listCache), true
}

func (s *Store) storeListCache(tasks []Task) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.listCache = cloneTasks(tasks)
	s.listValid = true
}

func (s *Store) storeTaskCache(t Task) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cloned := cloneTask(t)
	if !s.listValid {
		return
	}
	for i := range s.listCache {
		if s.listCache[i].ID != t.ID {
			continue
		}
		s.listCache[i] = cloned
		return
	}
	s.listCache = append(s.listCache, cloned)
}

func (s *Store) deleteCachedTask(id string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.listValid {
		return
	}
	for i := range s.listCache {
		if s.listCache[i].ID != id {
			continue
		}
		s.listCache = append(s.listCache[:i], s.listCache[i+1:]...)
		return
	}
	s.listValid = true
}

func (s *Store) invalidateListCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.listValid = false
}

func cloneTasks(tasks []Task) []Task {
	out := make([]Task, len(tasks))
	for i := range tasks {
		out[i] = cloneTask(tasks[i])
	}
	return out
}

func cloneTask(t Task) Task {
	clone := t
	clone.AllowedTools = slices.Clone(t.AllowedTools)
	clone.Tags = slices.Clone(t.Tags)
	clone.AgentRuns = slices.Clone(t.AgentRuns)
	if t.DueDate != nil {
		d := *t.DueDate
		clone.DueDate = &d
	}
	if t.ClosedAt != nil {
		c := *t.ClosedAt
		clone.ClosedAt = &c
	}
	if t.Workflow != nil {
		wfClone := cloneWorkflow(*t.Workflow)
		clone.Workflow = &wfClone
	}
	return clone
}

func cloneWorkflow(wf workflow.Execution) workflow.Execution {
	clone := wf
	clone.StepHistory = slices.Clone(wf.StepHistory)
	if wf.Variables != nil {
		clone.Variables = make(map[string]string, len(wf.Variables))
		maps.Copy(clone.Variables, wf.Variables)
	}
	if wf.CompletedAt != nil {
		ts := *wf.CompletedAt
		clone.CompletedAt = &ts
	}
	return clone
}

// UpdateMap converts raw to a typed Update and applies it.
// Returns an error on unknown keys or wrong value types.
func (s *Store) UpdateMap(id string, raw map[string]any) (Task, error) {
	u, err := UpdateFromMap(raw)
	if err != nil {
		return Task{}, err
	}
	return s.Update(id, u)
}

func (s *Store) AddRun(taskID string, run AgentRun) error {
	return s.addRun(taskID, run, nil)
}

func (s *Store) AddRunWithStatus(taskID string, run AgentRun, status *Status) error {
	return s.addRun(taskID, run, status)
}

func (s *Store) addRun(taskID string, run AgentRun, status *Status) error {
	t, err := s.Get(taskID)
	if err != nil {
		return err
	}
	if status != nil {
		oldStatus := t.Status
		t.Status = *status
		wasTerminal := IsTerminalStatus(oldStatus)
		isTerminal := IsTerminalStatus(t.Status)
		if !wasTerminal && isTerminal {
			now := time.Now().UTC()
			t.ClosedAt = &now
		} else if wasTerminal && !isTerminal {
			t.ClosedAt = nil
		}
	}
	t.AgentRuns = append(t.AgentRuns, run)
	d, err := Marshal(t)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(t.FilePath, d); err != nil {
		return err
	}
	s.storeTaskCache(t)
	return nil
}

func (s *Store) UpdateRun(taskID, agentID string, updates map[string]any) error {
	t, err := s.Get(taskID)
	if err != nil {
		return err
	}
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID != agentID {
			continue
		}
		if v, ok := updates["state"].(string); ok {
			t.AgentRuns[i].State = v
		}
		if v, ok := updates["cost_usd"].(float64); ok {
			t.AgentRuns[i].CostUSD = v
		}
		if v, ok := updates["result"].(string); ok {
			t.AgentRuns[i].Result = v
		}
		if v, ok := updates["log_file"].(string); ok {
			t.AgentRuns[i].LogFile = v
		}
		if v, ok := updates["session_id"].(string); ok && v != "" {
			t.AgentRuns[i].SessionID = v
		}
		break
	}
	d, err := Marshal(t)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(t.FilePath, d); err != nil {
		return err
	}
	s.storeTaskCache(t)
	return nil
}
