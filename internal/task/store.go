package task

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
	"github.com/google/uuid"
)

type Store struct {
	dir           string
	comments      *CommentStore
	plans         *PlanStore
	planCritiques *PlanCritiqueStore
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

func (s *Store) List() ([]Task, error) {
	paths, err := fsutil.ListFiles(s.dir, ".md")
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}

	var tasks []Task
	for _, p := range paths {
		t, err := Parse(p)
		if err != nil {
			slog.Default().Warn("task.parse.skip", "file", filepath.Base(p), "err", err)
			continue
		}
		t.Plan, _ = s.plans.Read(t.ID)
		t.PlanCritique, _ = s.planCritiques.Read(t.ID)
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *Store) Get(id string) (Task, error) {
	path := filepath.Join(s.dir, id+".md")
	t, err := Parse(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Task{}, fmt.Errorf("task %s not found", id)
		}
		return Task{}, err
	}
	t.Plan, _ = s.plans.Read(t.ID)
	t.PlanCritique, _ = s.planCritiques.Read(t.ID)
	return t, nil
}

func (s *Store) Create(title, body, mode string) (Task, error) {
	if mode == "" {
		mode = "interactive"
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
		t.Status = *u.Status
		// Clear reason when status changes unless a new reason is also provided.
		if u.StatusReason == nil {
			t.StatusReason = ""
		}
	}
	if u.StatusReason != nil {
		t.StatusReason = *u.StatusReason
	}
	if u.AgentMode != nil {
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

	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, fmt.Errorf("write task file: %w", err)
	}
	return t, nil
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
	t, err := s.Get(taskID)
	if err != nil {
		return err
	}
	t.AgentRuns = append(t.AgentRuns, run)
	d, err := Marshal(t)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(t.FilePath, d)
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
		break
	}
	d, err := Marshal(t)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(t.FilePath, d)
}
