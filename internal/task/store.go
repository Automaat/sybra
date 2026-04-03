package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Store struct {
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) List() ([]Task, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}

	var tasks []Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		t, err := Parse(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *Store) Get(id string) (Task, error) {
	tasks, err := s.List()
	if err != nil {
		return Task{}, err
	}
	for i := range tasks {
		if tasks[i].ID == id {
			return tasks[i], nil
		}
	}
	return Task{}, fmt.Errorf("task %s not found", id)
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
		Status:    StatusNew,
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
	if err := os.WriteFile(t.FilePath, data, 0o644); err != nil {
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
	return nil
}

func (s *Store) Update(id string, updates map[string]any) (Task, error) {
	t, err := s.Get(id)
	if err != nil {
		return Task{}, err
	}

	if v, ok := updates["title"].(string); ok {
		t.Title = v
	}
	if v, ok := updates["status"].(string); ok {
		t.Status = Status(v)
	}
	if v, ok := updates["agent_mode"].(string); ok {
		t.AgentMode = v
	}
	if v, ok := updates["body"].(string); ok {
		t.Body = v
	}
	if v, ok := updates["tags"].([]string); ok {
		t.Tags = v
	}
	if v, ok := updates["project_id"].(string); ok {
		t.ProjectID = v
	}
	if v, ok := updates["branch"].(string); ok {
		t.Branch = v
	}
	if v, ok := updates["pr_number"].(float64); ok {
		t.PRNumber = int(v)
	}

	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}
	if err := os.WriteFile(t.FilePath, data, 0o644); err != nil {
		return Task{}, fmt.Errorf("write task file: %w", err)
	}
	return t, nil
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
	return os.WriteFile(t.FilePath, d, 0o644)
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
	return os.WriteFile(t.FilePath, d, 0o644)
}
