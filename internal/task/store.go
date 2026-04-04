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

// atomicWrite writes data to path via a temp file + rename to prevent
// partial reads from concurrent goroutines.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
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
	path := filepath.Join(s.dir, id+".md")
	t, err := Parse(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Task{}, fmt.Errorf("task %s not found", id)
		}
		return Task{}, err
	}
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
	if err := atomicWrite(t.FilePath, data); err != nil {
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
		st, vErr := ValidateStatus(v)
		if vErr != nil {
			return Task{}, vErr
		}
		t.Status = st
	}
	if v, ok := updates["agent_mode"].(string); ok {
		t.AgentMode = v
	}
	if v, ok := updates["body"].(string); ok {
		t.Body = v
	}
	switch v := updates["tags"].(type) {
	case []string:
		t.Tags = v
	case string:
		t.Tags = strings.Split(v, ",")
	}
	if v, ok := updates["project_id"].(string); ok {
		t.ProjectID = v
	}
	if v, ok := updates["branch"].(string); ok {
		t.Branch = v
	}
	switch v := updates["pr_number"].(type) {
	case float64:
		t.PRNumber = int(v)
	case int:
		t.PRNumber = v
	}
	if v, ok := updates["reviewed"].(bool); ok {
		t.Reviewed = v
	}

	data, err := Marshal(t)
	if err != nil {
		return Task{}, err
	}
	if err := atomicWrite(t.FilePath, data); err != nil {
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
	return atomicWrite(t.FilePath, d)
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
	return atomicWrite(t.FilePath, d)
}
