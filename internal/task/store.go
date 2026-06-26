package task

import (
	"errors"
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
	planDrafts    *PlanDraftStore
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
		planDrafts:    NewPlanDraftStore(dir),
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

func (s *Store) PlanDrafts() *PlanDraftStore {
	return s.planDrafts
}

func (s *Store) PlanCritiques() *PlanCritiqueStore {
	return s.planCritiques
}

func (s *Store) CodeReviews() *CodeReviewStore {
	return s.codeReviews
}

// IsSidecarFile reports whether a filename (basename) belongs to a sidecar
// store rather than a primary task file. Centralized so adding a new
// sidecar kind only requires updating this list.
func IsSidecarFile(base string) bool {
	if IsPlanDraftFile(base) {
		return true
	}
	return strings.HasSuffix(base, ".plan.md") ||
		strings.HasSuffix(base, ".plan-critique.md") ||
		strings.HasSuffix(base, ".review.md")
}

func (s *Store) List() ([]Task, error) {
	if tasks, ok := s.cachedList(); ok {
		return tasks, nil
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read tasks dir: %w", err)
	}

	// Bucket entries in one pass so we don't ReadDir per-task in the
	// PlanDraftStore.List path (was N²) or stat-via-ENOENT 3 sidecars per
	// task (was 3N misses on the common no-sidecar case).
	var taskPaths []string
	sidecars := loadSidecarsFromEntries(s.dir, entries)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasSuffix(base, ".md") || IsSidecarFile(base) {
			continue
		}
		taskPaths = append(taskPaths, filepath.Join(s.dir, base))
	}

	tasks := make([]Task, 0, len(taskPaths))
	var parseErr bool
	for _, p := range taskPaths {
		t, err := Parse(p)
		if err != nil {
			slog.Default().Warn("task.parse.skip", "file", filepath.Base(p), "err", err)
			parseErr = true
			continue
		}
		t.Plan = sidecars.plans[t.ID]
		t.PlanCritique = sidecars.critiques[t.ID]
		t.CodeReview = sidecars.reviews[t.ID]
		if drafts, ok := sidecars.drafts[t.ID]; ok {
			t.PlanDrafts = drafts
		} else {
			t.PlanDrafts = map[string]string{}
		}
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

// sidecarIndex holds sidecar contents loaded in a single ReadDir pass,
// indexed by task ID. Used by List to amortize sidecar I/O.
type sidecarIndex struct {
	plans     map[string]string
	critiques map[string]string
	reviews   map[string]string
	drafts    map[string]map[string]string
}

// loadSidecarsFromEntries reads sidecar contents for every recognized
// suffix in a single pass. Read failures on individual sidecars are
// logged and skipped — matches the prior `_ = err` behavior of the
// per-task sidecar Reads in List, where a corrupt sidecar should not
// abort the whole task list.
func loadSidecarsFromEntries(dir string, entries []os.DirEntry) *sidecarIndex {
	idx := &sidecarIndex{
		plans:     map[string]string{},
		critiques: map[string]string{},
		reviews:   map[string]string{},
		drafts:    map[string]map[string]string{},
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasSuffix(base, ".md") {
			continue
		}
		// Order matters: plan-draft and plan-critique both have
		// ".plan" in them, so check the more specific suffix first.
		switch {
		case IsPlanDraftFile(base):
			// IsPlanDraftFile already guarantees the prefix is present,
			// but using Cut + the found flag keeps the lint clean and is
			// resilient if the helper's contract loosens later.
			id, rest, found := strings.Cut(base, PlanDraftSidecarPrefix)
			if !found {
				continue
			}
			name := strings.TrimSuffix(rest, ".md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			if idx.drafts[id] == nil {
				idx.drafts[id] = map[string]string{}
			}
			idx.drafts[id][name] = string(data)
		case strings.HasSuffix(base, ".plan-critique.md"):
			id := strings.TrimSuffix(base, ".plan-critique.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.critiques[id] = string(data)
		case strings.HasSuffix(base, ".plan.md"):
			id := strings.TrimSuffix(base, ".plan.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.plans[id] = string(data)
		case strings.HasSuffix(base, ".review.md"):
			id := strings.TrimSuffix(base, ".review.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.reviews[id] = string(data)
		}
	}
	return idx
}

func (s *Store) Get(id string) (Task, error) {
	t, err := s.read(id)
	if err != nil {
		return Task{}, err
	}
	t.Plan, _ = s.plans.Read(t.ID)
	t.PlanCritique, _ = s.planCritiques.Read(t.ID)
	t.CodeReview, _ = s.codeReviews.Read(t.ID)
	t.PlanDrafts, _ = s.planDrafts.List(t.ID)
	return t, nil
}

// read parses just the task file for id, skipping the sidecar fan-out that
// Get performs. Write paths (Update, Delete) and the watcher status hook only
// need the task's own frontmatter/body — the sidecar fields are yaml:"-" so
// Marshal never serializes them, and no List() consumer reads them off a
// cached entry. Loading them there is pure waste, dominated by
// PlanDraftStore.List, which scans the entire tasks dir (~one lstat per file)
// on every call. That scan was ~20% of server CPU and ~50% of allocations
// under churn because Update/Delete/OnExternalUpdate each paid it.
func (s *Store) read(id string) (Task, error) {
	path, err := s.safePath(id)
	if err != nil {
		return Task{}, err
	}
	t, err := Parse(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Wrap so callers can detect the missing file via
			// errors.Is(err, os.ErrNotExist) instead of string matching.
			return Task{}, fmt.Errorf("task %s not found: %w", id, err)
		}
		return Task{}, err
	}
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

// CreateFull persists a new task with optional initial field overrides applied
// atomically in the first write. Use this instead of Create+Update when the
// caller needs fields like RunRole, PRNumber, Tags, or ProjectID present before
// any file-watcher can read the task — avoiding the race where watcher picks up
// the bare task before the caller's Update applies.
func (s *Store) CreateFull(title, body, mode string, init Update) (Task, error) {
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
	// Apply initial field overrides before the first disk write so that any
	// watcher reading the file sees the complete task from the start.
	if init.ProjectID != nil {
		t.ProjectID = *init.ProjectID
	}
	if init.PRNumber != nil {
		t.PRNumber = *init.PRNumber
	}
	if init.Tags != nil {
		t.Tags = *init.Tags
	}
	if init.RunRole != nil {
		t.RunRole = *init.RunRole
	}
	if init.Body != nil {
		t.Body = *init.Body
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
	t, err := s.read(id)
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

func (s *Store) writeSidecars(id string, u Update, t *Task) error {
	if u.Plan != nil {
		if err := s.plans.Write(id, *u.Plan); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		t.Plan = *u.Plan
	}
	if u.PlanCritique != nil {
		if err := s.planCritiques.Write(id, *u.PlanCritique); err != nil {
			return fmt.Errorf("write plan critique: %w", err)
		}
		t.PlanCritique = *u.PlanCritique
	}
	if u.CodeReview != nil {
		if err := s.codeReviews.Write(id, *u.CodeReview); err != nil {
			return fmt.Errorf("write code review: %w", err)
		}
		t.CodeReview = *u.CodeReview
	}
	return nil
}

func (s *Store) Update(id string, u Update) (Task, error) {
	t, _, err := s.UpdateWithPrev(id, u)
	return t, err
}

// UpdateWithPrev applies u and returns both the updated task and the prior
// status. Lets callers (Manager.Update) wire status-change hooks without a
// redundant Get to read the previous value before the write.
// applyReviewFields copies the review-flow tracking fields (run role + inbound
// PR-review phase) from an Update onto a task. Split out of UpdateWithPrev to
// keep that function within the length budget.
func applyReviewFields(t *Task, u Update) {
	if u.RunRole != nil {
		t.RunRole = *u.RunRole
	}
	if u.Outcome != nil {
		t.Outcome = *u.Outcome
	}
	if u.MergeCommit != nil {
		t.MergeCommit = *u.MergeCommit
	}
	if u.ReviewPhase != nil {
		t.ReviewPhase = *u.ReviewPhase
	}
	if u.PRPhase != nil {
		t.PRPhase = *u.PRPhase
	}
}

// applyLinkFields applies the repo-location fields (project, branch, adopted
// worktree dir, PR, issue) that tie a task to its code.
func applyLinkFields(t *Task, u Update) {
	if u.ProjectID != nil {
		t.ProjectID = *u.ProjectID
	}
	if u.Branch != nil {
		t.Branch = *u.Branch
	}
	if u.WorktreeDir != nil {
		t.WorktreeDir = *u.WorktreeDir
	}
	if u.PRNumber != nil {
		t.PRNumber = *u.PRNumber
	}
	if u.Issue != nil {
		t.Issue = *u.Issue
	}
}

func (s *Store) UpdateWithPrev(id string, u Update) (Task, Status, error) {
	t, err := s.read(id)
	if err != nil {
		return Task{}, "", err
	}
	prevStatus := t.Status

	if u.Title != nil {
		t.Title = *u.Title
	}
	if u.Slug != nil {
		if err := ValidateSlug(*u.Slug); err != nil {
			return Task{}, "", err
		}
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
	if u.BlockedByIssue != nil {
		t.BlockedByIssue = *u.BlockedByIssue
	}
	if u.SupervisorSteer != nil {
		t.SupervisorSteer = *u.SupervisorSteer
	}
	if u.AgentMode != nil {
		if _, err := ValidateAgentMode(*u.AgentMode); err != nil {
			return Task{}, "", err
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
	applyLinkFields(&t, u)
	if u.Reviewed != nil {
		t.Reviewed = *u.Reviewed
	}
	applyReviewFields(&t, u)
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
	if u.MaxTurns != nil {
		t.MaxTurns = *u.MaxTurns
	}
	if u.ForkSubagent != nil {
		t.ForkSubagent = *u.ForkSubagent
	}
	if u.ReasoningEffort != nil {
		t.ReasoningEffort = *u.ReasoningEffort
	}
	if err := s.writeSidecars(id, u, &t); err != nil {
		return Task{}, "", err
	}

	data, err := Marshal(t)
	if err != nil {
		return Task{}, "", err
	}
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, "", fmt.Errorf("write task file: %w", err)
	}
	s.storeTaskCache(t)
	return t, prevStatus, nil
}

// InvalidatePath clears any cached task/list state for the given task file.
// Non-task files are ignored.
func (s *Store) InvalidatePath(path string) {
	if !strings.HasSuffix(path, ".md") {
		return
	}
	base := filepath.Base(path)
	if IsSidecarFile(base) {
		// An external plan-draft write/delete must drop the draft index so a
		// draft-less negative-cache hit can't mask a draft that appeared on
		// disk out-of-process.
		if IsPlanDraftFile(base) {
			s.planDrafts.invalidateIndex()
		}
		s.invalidateListCache()
		return
	}
	id := strings.TrimSuffix(base, ".md")
	if id == "" {
		return
	}
	// Targeted refresh instead of a blanket invalidate: a single task file
	// changed (commonly the fsnotify echo of our OWN AtomicWrite ~200ms
	// earlier), so re-read just that file and patch its one cache entry rather
	// than dropping the whole list and forcing the next List() to re-parse and
	// re-clone every task. Keeps the list cache warm under active agent write
	// load, where it was perpetually cold.
	s.refreshCachedTask(id)
}

// refreshCachedTask re-reads a single task (with sidecars, so List output is
// identical to a full rebuild) and patches its entry in the warm list cache.
// A vanished file removes the entry; an unexpected read error falls back to a
// full invalidate. No-op when the cache is cold (storeTaskCache guards on it).
func (s *Store) refreshCachedTask(id string) {
	t, err := s.Get(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.deleteCachedTask(id)
			return
		}
		s.invalidateListCache()
		return
	}
	s.storeTaskCache(t)
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
	// Deep-copy ParallelInflight: the outer map and every *ParallelChildren +
	// nested *ChildStatus must be independent. Without this, a Task fetched
	// via List() shares the in-flight bookkeeping with the listCache entry,
	// so a caller that mutates wf.ParallelInflight on a returned clone
	// silently corrupts cached state — and any subsequent List() observes the
	// torn maps until the cache is invalidated.
	if wf.ParallelInflight != nil {
		clone.ParallelInflight = make(map[string]*workflow.ParallelChildren, len(wf.ParallelInflight))
		for k, v := range wf.ParallelInflight {
			if v == nil {
				clone.ParallelInflight[k] = nil
				continue
			}
			pcClone := *v
			if v.Children != nil {
				pcClone.Children = make(map[string]*workflow.ChildStatus, len(v.Children))
				for ck, cv := range v.Children {
					if cv == nil {
						pcClone.Children[ck] = nil
						continue
					}
					csClone := *cv
					pcClone.Children[ck] = &csClone
				}
			}
			clone.ParallelInflight[k] = &pcClone
		}
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
		if v, ok := updates["verdict"].(string); ok && v != "" {
			t.AgentRuns[i].Verdict = v
		}
		if v, ok := updates["log_file"].(string); ok {
			t.AgentRuns[i].LogFile = v
		}
		if v, ok := updates["session_id"].(string); ok && v != "" {
			t.AgentRuns[i].SessionID = v
		}
		if v, ok := updates["head_sha"].(string); ok && v != "" {
			t.AgentRuns[i].HeadSHA = v
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
