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

// Store is the filesystem-backed CRUD layer for task markdown files under
// dir (tasks/<id>.md). It also owns the sidecar stores for planning,
// review, and comment content that lives in adjacent files rather than the
// task's own frontmatter+body. Safe for concurrent use within a process; see
// lockTask for the cross-process locking story.
type Store struct {
	dir           string
	trashDir      string
	comments      *CommentStore
	plans         *PlanStore
	planContracts *PlanningSidecarStore
	planDrafts    *PlanDraftStore
	planCritiques *PlanCritiqueStore
	planResearch  *PlanningSidecarStore
	planDecisions *PlanningSidecarStore
	planBrief     *PlanningSidecarStore
	codeReviews   *CodeReviewStore
	writeLocksMu  sync.Mutex
	writeLocks    map[string]*taskWriteLock
	cacheMu       sync.RWMutex
	listCache     []Task
	listValid     bool
}

type taskWriteLock struct {
	mu   sync.Mutex
	refs int
}

// NewStore creates dir if it does not exist and returns a Store rooted
// there, along with its sidecar stores (comments, plans, code reviews).
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}
	return &Store{
		dir:           dir,
		trashDir:      filepath.Join(filepath.Dir(dir), "trash"),
		comments:      NewCommentStore(dir),
		plans:         NewPlanStore(dir),
		planContracts: NewPlanningSidecarStore(dir, ".plan-contract.json", "plan contract"),
		planDrafts:    NewPlanDraftStore(dir),
		planCritiques: NewPlanCritiqueStore(dir),
		planResearch:  NewPlanningSidecarStore(dir, ".plan-research.md", "plan research"),
		planDecisions: NewPlanningSidecarStore(dir, ".plan-decisions.md", "plan decisions"),
		planBrief:     NewPlanningSidecarStore(dir, ".plan-brief.md", "plan brief"),
		codeReviews:   NewCodeReviewStore(dir),
		writeLocks:    map[string]*taskWriteLock{},
	}, nil
}

// Comments returns the sidecar store for per-task comment threads.
func (s *Store) Comments() *CommentStore {
	return s.comments
}

// Dir returns the root tasks directory backing this store.
func (s *Store) Dir() string {
	return s.dir
}

// TrashDir returns the directory soft-deleted tasks are moved into by
// Delete, sibling to the tasks dir (e.g. ~/.sybra/trash for
// ~/.sybra/tasks). Exposed for CLI diagnostics and tests.
func (s *Store) TrashDir() string {
	return s.trashDir
}

// Plans returns the sidecar store for the human-readable compact plan
// (Task.Plan).
func (s *Store) Plans() *PlanStore {
	return s.plans
}

// PlanContracts returns the sidecar store for the machine-validated JSON
// plan contract (Task.PlanContract) that implementation agents consume.
func (s *Store) PlanContracts() *PlanningSidecarStore {
	return s.planContracts
}

// PlanDrafts returns the sidecar store for per-provider raw plan output
// (Task.PlanDrafts) produced during dual/N-provider planning.
func (s *Store) PlanDrafts() *PlanDraftStore {
	return s.planDrafts
}

// PlanCritiques returns the sidecar store for plan-critic review output
// (Task.PlanCritique).
func (s *Store) PlanCritiques() *PlanCritiqueStore {
	return s.planCritiques
}

// PlanResearch returns the sidecar store for planning research/evidence
// material (Task.PlanResearch).
func (s *Store) PlanResearch() *PlanningSidecarStore {
	return s.planResearch
}

// PlanDecisions returns the sidecar store for planning decision-log content
// (Task.PlanDecisions).
func (s *Store) PlanDecisions() *PlanningSidecarStore {
	return s.planDecisions
}

// PlanBrief returns the sidecar store for the condensed planning brief
// (Task.PlanBrief).
func (s *Store) PlanBrief() *PlanningSidecarStore {
	return s.planBrief
}

// CodeReviews returns the sidecar store for code-review output
// (Task.CodeReview).
func (s *Store) CodeReviews() *CodeReviewStore {
	return s.codeReviews
}

// lockTask serializes read/modify/write calls for a single task file, both
// within this process and across others. sybra-cli and the recovery sweep
// run task.Store in separate OS processes from the GUI server against the
// same tasks dir, so the in-process sync.Mutex alone cannot prevent two
// processes from interleaving a read and a write and silently dropping
// fields. The in-process lock is acquired first (cheap, avoids paying flock
// syscalls for intra-process contention) and the cross-process flock second;
// they are released in reverse order. It is deliberately ref-counted so
// deleted or one-off task IDs do not leave lock entries behind for the
// lifetime of a long-running server.
//
// If the flock cannot be acquired, the write must not proceed with only the
// in-process mutex held — that would silently reintroduce the cross-process
// race this lock exists to close — so the in-process lock is released and an
// error is returned instead.
func (s *Store) lockTask(id string) (func(), error) {
	s.writeLocksMu.Lock()
	if s.writeLocks == nil {
		s.writeLocks = map[string]*taskWriteLock{}
	}
	lock := s.writeLocks[id]
	if lock == nil {
		lock = &taskWriteLock{}
		s.writeLocks[id] = lock
	}
	lock.refs++
	s.writeLocksMu.Unlock()

	lock.mu.Lock()

	releaseInProcess := func() {
		lock.mu.Unlock()
		s.writeLocksMu.Lock()
		defer s.writeLocksMu.Unlock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.writeLocks, id)
		}
	}

	path, err := s.safePath(id)
	if err != nil {
		releaseInProcess()
		return nil, err
	}
	unlockFile, err := fsutil.LockFile(path)
	if err != nil {
		releaseInProcess()
		return nil, fmt.Errorf("lock task %s: %w", id, err)
	}

	return func() {
		if err := unlockFile(); err != nil {
			slog.Default().Warn("task.lockTask.unlock_failed", "id", id, "err", err)
		}
		releaseInProcess()
	}, nil
}

// sidecarFileSuffixes lists every fixed-name (non-plan-draft) sidecar
// suffix a task can own. Single source of truth for both IsSidecarFile and
// Store.taskFiles, so a new fixed-name sidecar kind only needs to be added
// here. Plan drafts use caller-chosen names (see IsPlanDraftFile) and so
// can't be enumerated as fixed suffixes.
var sidecarFileSuffixes = []string{
	".comments.json",
	".plan.md",
	".plan-contract.json",
	".plan-critique.md",
	".plan-research.md",
	".plan-decisions.md",
	".plan-brief.md",
	".review.md",
}

// IsSidecarFile reports whether a filename (basename) belongs to a sidecar
// store rather than a primary task file. Centralized so adding a new
// sidecar kind only requires updating sidecarFileSuffixes (or, for
// caller-named sidecars like plan drafts, IsPlanDraftFile).
func IsSidecarFile(base string) bool {
	if IsPlanDraftFile(base) {
		return true
	}
	for _, suffix := range sidecarFileSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// List returns every task under the store's directory, in directory-read
// order (unspecified — sort by CreatedAt/UpdatedAt if you need an order).
// A task with a parse error is logged and skipped rather than failing the
// whole call. Results are served from an in-memory cache invalidated on any
// Create/Update/Delete; callers do not need to worry about staleness within
// a single process.
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
		t.PlanContract = sidecars.contracts[t.ID]
		t.PlanCritique = sidecars.critiques[t.ID]
		t.PlanResearch = sidecars.research[t.ID]
		t.PlanDecisions = sidecars.decisions[t.ID]
		t.PlanBrief = sidecars.briefs[t.ID]
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
	contracts map[string]string
	critiques map[string]string
	research  map[string]string
	decisions map[string]string
	briefs    map[string]string
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
		contracts: map[string]string{},
		critiques: map[string]string{},
		research:  map[string]string{},
		decisions: map[string]string{},
		briefs:    map[string]string{},
		reviews:   map[string]string{},
		drafts:    map[string]map[string]string{},
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasSuffix(base, ".md") && !strings.HasSuffix(base, ".json") {
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
		case strings.HasSuffix(base, ".plan-contract.json"):
			id := strings.TrimSuffix(base, ".plan-contract.json")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.contracts[id] = string(data)
		case strings.HasSuffix(base, ".plan-research.md"):
			id := strings.TrimSuffix(base, ".plan-research.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.research[id] = string(data)
		case strings.HasSuffix(base, ".plan-decisions.md"):
			id := strings.TrimSuffix(base, ".plan-decisions.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.decisions[id] = string(data)
		case strings.HasSuffix(base, ".plan-brief.md"):
			id := strings.TrimSuffix(base, ".plan-brief.md")
			data, err := os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
				continue
			}
			idx.briefs[id] = string(data)
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

// Get reads task id and populates its planning/review sidecar fields
// (Plan, PlanContract, PlanCritique, PlanResearch, PlanDecisions, PlanBrief,
// CodeReview, PlanDrafts) from their sidecar files. Returns an error
// satisfying errors.Is(err, os.ErrNotExist) when id has no task file.
func (s *Store) Get(id string) (Task, error) {
	t, err := s.read(id)
	if err != nil {
		return Task{}, err
	}
	t.Plan, _ = s.plans.Read(t.ID)
	t.PlanContract, _ = s.planContracts.Read(t.ID)
	t.PlanCritique, _ = s.planCritiques.Read(t.ID)
	t.PlanResearch, _ = s.planResearch.Read(t.ID)
	t.PlanDecisions, _ = s.planDecisions.Read(t.ID)
	t.PlanBrief, _ = s.planBrief.Read(t.ID)
	t.CodeReview, _ = s.codeReviews.Read(t.ID)
	t.PlanDrafts, _ = s.planDrafts.List(t.ID)
	return t, nil
}

// read parses just the task file for id, skipping the sidecar fan-out that
// Get performs. Write paths (Update, Delete) and the watcher status hook only
// need the task's own frontmatter/body — sidecars live outside the task
// frontmatter schema, and no List() consumer reads them off a cached entry.
// Loading them there is pure waste, dominated by
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

// Create writes a new task file with a fresh 8-char ID, status "todo", and
// type "normal". mode defaults to AgentModeInteractive when empty and is
// validated via ValidateAgentMode. Use CreateFull to set additional fields
// (tags, project, priority, ...) atomically at creation time.
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
// atomically in the first task-file write. Use this instead of Create+Update
// when the caller needs fields like RunRole, PRNumber, Tags, ProjectID,
// WorktreeDir, or Plan present before any file-watcher can read the task —
// avoiding the race where watcher picks up the bare task before the caller's
// Update applies.
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
	applyCreateInit(&t, init, now)
	if err := s.writeSidecars(t.ID, init, &t); err != nil {
		return Task{}, err
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

// applyCreateInit applies the CreateFull init overrides onto a fresh task.
// Split out of CreateFull so the create path stays within the length budget as
// new initializable fields are added. Link fields (project, branch, worktree,
// PR, issue, umbrella, depends_on) reuse applyLinkFields so the create and
// update paths cannot drift.
func applyCreateInit(t *Task, init Update, now time.Time) {
	applyLinkFields(t, init)
	if init.Tags != nil {
		t.Tags = *init.Tags
	}
	if init.RunRole != nil {
		t.RunRole = *init.RunRole
	}
	if init.Status != nil {
		t.Status = *init.Status
		if IsTerminalStatus(t.Status) {
			closedAt := now
			t.ClosedAt = &closedAt
		}
	}
	if init.StatusReason != nil {
		t.StatusReason = *init.StatusReason
	}
	if init.Body != nil {
		t.Body = *init.Body
	}
	if init.Reviewed != nil {
		t.Reviewed = *init.Reviewed
	}
	if init.TaskType != nil {
		t.TaskType = *init.TaskType
	}
	if init.MaxTurns != nil {
		t.MaxTurns = *init.MaxTurns
	}
	if init.ForkSubagent != nil {
		t.ForkSubagent = *init.ForkSubagent
	}
	if init.Sandbox != nil {
		t.Sandbox = init.Sandbox
	}
	if init.ReasoningEffort != nil {
		t.ReasoningEffort = *init.ReasoningEffort
	}
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

// taskFiles returns the basenames of every sidecar file this store owns for
// id (comments, plans, contracts, critiques, research, decisions, brief,
// code reviews, plan drafts) — the primary "<id>.md" is not included, since
// Delete/RestoreFromTrash handle it separately. Checks each fixed-name
// suffix in sidecarFileSuffixes directly (bounded, ~8 stat calls) rather
// than scanning the whole tasks directory, so cost scales with the sidecar
// kinds that exist, not with the number of live tasks in the store. Plan
// drafts go through planDrafts.List, which has its own negative-cache index
// and so is also O(1) for the common case of a task with no drafts.
func (s *Store) taskFiles(id string) ([]string, error) {
	if _, err := s.safePath(id); err != nil {
		return nil, err
	}
	var out []string
	for _, suffix := range sidecarFileSuffixes {
		base := id + suffix
		if _, err := os.Stat(filepath.Join(s.dir, base)); err == nil {
			out = append(out, base)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", base, err)
		}
	}
	drafts, err := s.planDrafts.List(id)
	if err != nil {
		return nil, fmt.Errorf("list plan drafts: %w", err)
	}
	for name := range drafts {
		out = append(out, id+PlanDraftSidecarPrefix+name+".md")
	}
	return out, nil
}

// newTrashGeneration creates and returns a fresh, empty directory under
// trashDir/<yyyy-mm-dd>/ to hold one Delete's worth of files for id. The
// first delete of id on a given date uses the bare id as the generation
// name; same-day redeletes (delete → restore → delete again) get a
// "<id>--<HHMMSS>-<nn>" suffix so they don't collide with — or silently
// overwrite — the earlier generation.
func (s *Store) newTrashGeneration(id string) (string, error) {
	dateDir := filepath.Join(s.trashDir, time.Now().UTC().Format(time.DateOnly))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return "", fmt.Errorf("create trash date dir: %w", err)
	}
	base := filepath.Join(dateDir, id)
	if err := os.Mkdir(base, 0o755); err == nil {
		return base, nil
	} else if !os.IsExist(err) {
		return "", err
	}
	for n := 1; ; n++ {
		candidate := filepath.Join(dateDir, fmt.Sprintf("%s--%s-%02d", id, time.Now().UTC().Format("150405"), n))
		if err := os.Mkdir(candidate, 0o755); err == nil {
			return candidate, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
}

// Delete moves task id's file, along with every sidecar it may own
// (comments, plans, contracts, critiques, research, decisions, brief, code
// reviews, plan drafts), into a dated generation directory under the trash
// dir instead of unlinking them — see TrashDir. Sidecars are moved before
// the primary "<id>.md" file so that a crash or rename failure partway
// through leaves the primary file as the source of truth for whether the
// task still "exists": either it's still in place (delete didn't complete)
// or it's gone (delete completed). If any rename fails partway through,
// every file already moved into genDir is rolled back to s.dir and genDir
// is removed, so a partial failure never leaves an orphaned generation
// (unlistable/unrestorable/unprunable — trashGenerationID requires the
// primary file to identify a generation) or a live task missing sidecars.
// There is deliberately no copy/delete fallback if a rename itself fails
// (e.g. trash on a different filesystem) — that error is returned as-is.
func (s *Store) Delete(id string) error {
	unlock, err := s.lockTask(id)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(id)
	if err != nil {
		return err
	}
	files, err := s.taskFiles(id)
	if err != nil {
		return err
	}
	genDir, err := s.newTrashGeneration(id)
	if err != nil {
		return fmt.Errorf("create trash generation: %w", err)
	}
	primary := id + ".md"
	moved := make([]string, 0, len(files))
	rollback := func() {
		for _, base := range moved {
			_ = os.Rename(filepath.Join(genDir, base), filepath.Join(s.dir, base))
		}
		_ = os.Remove(genDir)
	}
	for _, base := range files {
		if err := os.Rename(filepath.Join(s.dir, base), filepath.Join(genDir, base)); err != nil {
			rollback()
			return fmt.Errorf("move %s to trash: %w", base, err)
		}
		moved = append(moved, base)
	}
	if err := os.Rename(t.FilePath, filepath.Join(genDir, primary)); err != nil {
		rollback()
		return fmt.Errorf("move task file to trash: %w", err)
	}
	s.planDrafts.invalidateIndex()
	s.deleteCachedTask(id)
	return nil
}

// TrashEntry describes one soft-deleted task generation for ListTrash and
// PruneTrash callers (CLI table/JSON output, prune logging).
type TrashEntry struct {
	ID          string    `json:"id"`
	Generation  string    `json:"generation"`
	DeletedDate string    `json:"deleted_date"`
	DeletedAt   time.Time `json:"deleted_at"`
	Title       string    `json:"title"`
}

// trashGenerationID returns the task id owned by a trash generation
// directory, identified by the one file inside it that ends in ".md" but is
// not itself a sidecar (IsSidecarFile) — i.e. the primary "<id>.md". This
// avoids relying on the generation directory's name (which is a convenience
// naming scheme, not a contract) to recover id, so list/restore/prune never
// prefix-match an arbitrary id. ok is false for an empty, already-restored,
// or corrupt generation. Read errors are returned so callers can surface
// unreadable generations instead of silently skipping them.
func trashGenerationID(genDir string) (id string, ok bool, err error) {
	entries, err := os.ReadDir(genDir)
	if err != nil {
		return "", false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if strings.HasSuffix(base, ".md") && !IsSidecarFile(base) {
			return strings.TrimSuffix(base, ".md"), true, nil
		}
	}
	return "", false, nil
}

// walkTrashGenerations calls fn for every generation directory under
// trashDir/<yyyy-mm-dd>/, passing the deletion date, the generation
// directory's basename, and its full path. fn's error stops the walk for
// that generation only (recorded via the returned slice) — one unreadable
// generation must not abort ListTrash/PruneTrash for the rest.
func (s *Store) walkTrashGenerations(fn func(date, generation, path string) error) []error {
	var errs []error
	dateEntries, err := os.ReadDir(s.trashDir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("read trash dir: %w", err))
		}
		return errs
	}
	for _, de := range dateEntries {
		if !de.IsDir() {
			continue
		}
		date := de.Name()
		dateDir := filepath.Join(s.trashDir, date)
		genEntries, err := os.ReadDir(dateDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read trash date dir %s: %w", date, err))
			continue
		}
		for _, ge := range genEntries {
			if !ge.IsDir() {
				continue
			}
			if err := fn(date, ge.Name(), filepath.Join(dateDir, ge.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// trashEntryFor builds a TrashEntry for a generation directory, reading its
// title from the trashed primary file (best-effort — a parse failure leaves
// Title empty rather than failing the whole entry).
func trashEntryFor(date, generation, path, id string) TrashEntry {
	entry := TrashEntry{ID: id, Generation: generation, DeletedDate: date}
	if info, err := os.Stat(path); err == nil {
		entry.DeletedAt = info.ModTime()
	}
	if t, err := Parse(filepath.Join(path, id+".md")); err == nil {
		entry.Title = t.Title
	}
	return entry
}

// ListTrash returns one entry per trashed generation across all dates,
// newest first. Multiple generations for the same id (repeated
// delete/restore cycles) each get their own entry so callers can
// distinguish them.
func (s *Store) ListTrash() ([]TrashEntry, error) {
	var out []TrashEntry
	errs := s.walkTrashGenerations(func(date, generation, path string) error {
		id, ok, err := trashGenerationID(path)
		if err != nil {
			return fmt.Errorf("read trash generation %s/%s: %w", date, generation, err)
		}
		if !ok {
			return nil
		}
		out = append(out, trashEntryFor(date, generation, path, id))
		return nil
	})
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	slices.SortFunc(out, func(a, b TrashEntry) int {
		return b.DeletedAt.Compare(a.DeletedAt)
	})
	return out, nil
}

// newestGeneration returns the path to the most recently deleted trash
// generation owned by id, or "" if none exist.
func (s *Store) newestGeneration(id string) (string, error) {
	var newest string
	var newestAt time.Time
	errs := s.walkTrashGenerations(func(date, generation, path string) error {
		gid, ok, err := trashGenerationID(path)
		if err != nil {
			return fmt.Errorf("read trash generation %s/%s: %w", date, generation, err)
		}
		if !ok || gid != id {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("stat trash generation %s: %w", path, statErr)
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest = path
			newestAt = info.ModTime()
		}
		return nil
	})
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return newest, nil
}

// removeEmptyTrashDirs removes genDir (expected empty after a restore) and,
// if that leaves its parent date directory empty too, removes that as well.
func removeEmptyTrashDirs(genDir string) {
	if err := os.Remove(genDir); err != nil {
		return
	}
	dateDir := filepath.Dir(genDir)
	entries, err := os.ReadDir(dateDir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(dateDir)
	}
}

// RestoreFromTrash moves id's newest trash generation back into the tasks
// dir under the same per-task lock Delete uses, restoring sidecars before
// the primary "<id>.md" file (mirror of Delete's ordering). If any rename
// fails partway through, every file already restored to s.dir is rolled
// back into genDir so a partial failure never leaves orphaned "<id>.*"
// sidecars in the live tasks dir with no primary file — such litter would
// be invisible to List, ListTrash, and PruneTrash, and unreachable by a
// future Delete(id), which requires the primary file to exist. Refuses if
// a live task with id already exists rather than silently overwriting it.
func (s *Store) RestoreFromTrash(id string) (Task, error) {
	unlock, err := s.lockTask(id)
	if err != nil {
		return Task{}, err
	}
	defer unlock()

	livePath, err := s.safePath(id)
	if err != nil {
		return Task{}, err
	}
	if _, err := os.Stat(livePath); err == nil {
		return Task{}, fmt.Errorf("task %s already exists, refusing to overwrite with trashed copy", id)
	} else if !os.IsNotExist(err) {
		return Task{}, fmt.Errorf("stat live task: %w", err)
	}

	genDir, err := s.newestGeneration(id)
	if err != nil {
		return Task{}, err
	}
	if genDir == "" {
		return Task{}, fmt.Errorf("no trashed task found for id %s: %w", id, os.ErrNotExist)
	}

	entries, err := os.ReadDir(genDir)
	if err != nil {
		return Task{}, fmt.Errorf("read trash generation: %w", err)
	}
	primary := id + ".md"
	restored := make([]string, 0, len(entries))
	rollback := func() {
		for _, base := range restored {
			_ = os.Rename(filepath.Join(s.dir, base), filepath.Join(genDir, base))
		}
	}
	for _, e := range entries {
		base := e.Name()
		if base == primary {
			continue
		}
		if err := os.Rename(filepath.Join(genDir, base), filepath.Join(s.dir, base)); err != nil {
			rollback()
			return Task{}, fmt.Errorf("restore %s: %w", base, err)
		}
		restored = append(restored, base)
	}
	if err := os.Rename(filepath.Join(genDir, primary), livePath); err != nil {
		rollback()
		return Task{}, fmt.Errorf("restore task file: %w", err)
	}
	removeEmptyTrashDirs(genDir)

	s.planDrafts.invalidateIndex()
	t, err := s.Get(id)
	if err != nil {
		return Task{}, err
	}
	s.storeTaskCache(t)
	return t, nil
}

// TrashPruneReport summarizes one PruneTrash run.
type TrashPruneReport struct {
	Scanned int
	Removed int
	Entries []TrashEntry
	Errors  []error
}

// pruneGeneration removes genDir under id's per-task lock, rechecking that
// it still exists once the lock is held — a concurrent RestoreFromTrash may
// have already moved it out from under the unlocked scan in PruneTrash.
// removed is false (with a nil error) when the recheck lost the race, so
// callers can distinguish "actually deleted" from "already gone" instead of
// double-counting a generation a concurrent restore already consumed.
func (s *Store) pruneGeneration(id, genDir string) (removed bool, err error) {
	unlock, err := s.lockTask(id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Stat(genDir); os.IsNotExist(err) {
		return false, nil
	}
	if err := os.RemoveAll(genDir); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteTrashedGeneration permanently removes id's newest trashed
// generation right away, bypassing the retention window. This is the
// escape hatch PruneTrash can't provide: a compliance request or a leaked
// credential needs trashed content gone immediately, not after
// RetentionDays or the next prune sweep.
func (s *Store) DeleteTrashedGeneration(id string) (bool, error) {
	genDir, err := s.newestGeneration(id)
	if err != nil {
		return false, err
	}
	if genDir == "" {
		return false, fmt.Errorf("no trashed task found for id %s: %w", id, os.ErrNotExist)
	}
	return s.pruneGeneration(id, genDir)
}

// PruneTrash permanently removes trash generations deleted more than
// retentionDays ago. A negative retentionDays disables pruning entirely
// (no-op). Each generation is removed under its own id's per-task lock, so
// pruning can never race a concurrent restore of the same id — unlike a
// naive "remove the whole date directory" sweep, which could not take a
// single lock covering every id it touches.
func (s *Store) PruneTrash(retentionDays int) (TrashPruneReport, error) {
	if retentionDays < 0 {
		return TrashPruneReport{}, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.DateOnly)
	return s.pruneTrashBefore(cutoff)
}

// PruneAllTrash permanently removes every trash generation regardless of
// age — the "empty the trash now" counterpart to PruneTrash's
// retention-gated sweep, for a `trash empty` CLI command.
func (s *Store) PruneAllTrash() (TrashPruneReport, error) {
	// No real deletion date sorts >= this, so every date dir qualifies.
	return s.pruneTrashBefore("9999-99-99")
}

// pruneTrashBefore is the shared body of PruneTrash and PruneAllTrash: it
// removes every trash generation dated strictly before cutoff (a
// time.DateOnly string).
func (s *Store) pruneTrashBefore(cutoff string) (TrashPruneReport, error) {
	var rep TrashPruneReport
	dateEntries, err := os.ReadDir(s.trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, fmt.Errorf("read trash dir: %w", err)
	}
	for _, de := range dateEntries {
		if !de.IsDir() || de.Name() >= cutoff {
			continue
		}
		date := de.Name()
		dateDir := filepath.Join(s.trashDir, date)
		genEntries, err := os.ReadDir(dateDir)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("read trash date dir %s: %w", date, err))
			continue
		}
		for _, ge := range genEntries {
			if !ge.IsDir() {
				continue
			}
			rep.Scanned++
			genDir := filepath.Join(dateDir, ge.Name())
			id, ok, err := trashGenerationID(genDir)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("read trash generation %s/%s: %w", date, ge.Name(), err))
				continue
			}
			if !ok {
				continue
			}
			entry := trashEntryFor(date, ge.Name(), genDir, id)
			removed, err := s.pruneGeneration(id, genDir)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("prune %s/%s: %w", date, ge.Name(), err))
				continue
			}
			if !removed {
				continue
			}
			rep.Removed++
			rep.Entries = append(rep.Entries, entry)
		}
		if remaining, err := os.ReadDir(dateDir); err == nil && len(remaining) == 0 {
			_ = os.Remove(dateDir)
		}
	}
	return rep, nil
}

func (s *Store) writeSidecars(id string, u Update, t *Task) error {
	if u.Plan != nil {
		if err := s.plans.Write(id, *u.Plan); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		t.Plan = *u.Plan
	}
	if u.PlanContract != nil {
		if err := s.planContracts.Write(id, *u.PlanContract); err != nil {
			return fmt.Errorf("write plan contract: %w", err)
		}
		t.PlanContract = *u.PlanContract
	}
	if u.PlanCritique != nil {
		if err := s.planCritiques.Write(id, *u.PlanCritique); err != nil {
			return fmt.Errorf("write plan critique: %w", err)
		}
		t.PlanCritique = *u.PlanCritique
	}
	if u.PlanResearch != nil {
		if err := s.planResearch.Write(id, *u.PlanResearch); err != nil {
			return fmt.Errorf("write plan research: %w", err)
		}
		t.PlanResearch = *u.PlanResearch
	}
	if u.PlanDecisions != nil {
		if err := s.planDecisions.Write(id, *u.PlanDecisions); err != nil {
			return fmt.Errorf("write plan decisions: %w", err)
		}
		t.PlanDecisions = *u.PlanDecisions
	}
	if u.PlanBrief != nil {
		if err := s.planBrief.Write(id, *u.PlanBrief); err != nil {
			return fmt.Errorf("write plan brief: %w", err)
		}
		t.PlanBrief = *u.PlanBrief
	}
	if u.CodeReview != nil {
		if err := s.codeReviews.Write(id, *u.CodeReview); err != nil {
			return fmt.Errorf("write code review: %w", err)
		}
		t.CodeReview = *u.CodeReview
	}
	return nil
}

// Update applies a partial field update u to task id and returns the
// updated task. Only non-nil fields of u are applied; see UpdateWithPrev if
// you also need the task's status before the update.
func (s *Store) Update(id string, u Update) (Task, error) {
	t, _, err := s.UpdateWithPrev(id, u)
	return t, err
}

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
	if u.HandoffSourceProvider != nil {
		t.HandoffSourceProvider = *u.HandoffSourceProvider
	}
	if u.PRNumber != nil {
		t.PRNumber = *u.PRNumber
	}
	if u.Issue != nil {
		t.Issue = *u.Issue
	}
	if u.RefIssue != nil {
		t.RefIssue = *u.RefIssue
	}
	if u.UmbrellaIssue != nil {
		t.UmbrellaIssue = *u.UmbrellaIssue
	}
	if u.DependsOn != nil {
		t.DependsOn = slices.Clone(*u.DependsOn)
	}
}

func applyUpdateFields(t *Task, u Update) error {
	if u.Title != nil {
		t.Title = *u.Title
	}
	if u.Slug != nil {
		if err := ValidateSlug(*u.Slug); err != nil {
			return err
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
			return err
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
	applyLinkFields(t, u)
	if u.Reviewed != nil {
		t.Reviewed = *u.Reviewed
	}
	applyReviewFields(t, u)
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
	if u.Sandbox != nil {
		t.Sandbox = u.Sandbox
	}
	if u.ReasoningEffort != nil {
		t.ReasoningEffort = *u.ReasoningEffort
	}
	if u.TestingCycleStartedAt != nil {
		t.TestingCycleStartedAt = u.TestingCycleStartedAt
	}
	return nil
}

// UpdateWithPrev applies u under the per-task write lock and returns both
// the updated task and its status before the update. Lets callers
// (Manager.Update) wire status-change hooks without a redundant Get to read
// the previous value before the write.
func (s *Store) UpdateWithPrev(id string, u Update) (Task, Status, error) {
	unlock, err := s.lockTask(id)
	if err != nil {
		return Task{}, "", err
	}
	defer unlock()

	t, err := s.read(id)
	if err != nil {
		return Task{}, "", err
	}
	prevStatus := t.Status
	if err := applyUpdateFields(&t, u); err != nil {
		return Task{}, "", err
	}
	// When any caller (CLI, GUI, API) moves the task out of human-required,
	// stamp TestingCycleStartedAt so route_test_result only counts runs from
	// this new cycle, not from prior ones. Skipped when the caller already
	// supplied an explicit value (u.TestingCycleStartedAt != nil).
	now := time.Now().UTC()
	if prevStatus == StatusHumanRequired &&
		t.Status != StatusHumanRequired &&
		u.TestingCycleStartedAt == nil {
		t.TestingCycleStartedAt = &now
	}
	t.UpdatedAt = now
	t.TamperFlagged = isTamperFlagged(t.Status, t.StatusReason)
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
	if u.writesSidecar() {
		full, err := s.Get(id)
		if err != nil {
			return Task{}, "", err
		}
		s.storeTaskCache(full)
		return full, prevStatus, nil
	}
	s.storeTaskCache(t)
	return t, prevStatus, nil
}

// InvalidatePath clears any cached task/list state for the given task file.
// Non-task files are ignored.
func (s *Store) InvalidatePath(path string) {
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
	if !strings.HasSuffix(base, ".md") {
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
	clone.DependsOn = slices.Clone(t.DependsOn)
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
	if wf.StepCounts != nil {
		clone.StepCounts = make(map[string]int, len(wf.StepCounts))
		maps.Copy(clone.StepCounts, wf.StepCounts)
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

// AddRun appends run to taskID's AgentRuns without changing its status.
func (s *Store) AddRun(taskID string, run AgentRun) error {
	return s.addRun(taskID, run, nil)
}

// AddRunWithStatus appends run to taskID's AgentRuns and atomically sets its
// status to *status. Use this instead of AddRun+Update when the status
// transition must be recorded alongside the run that caused it.
func (s *Store) AddRunWithStatus(taskID string, run AgentRun, status *Status) error {
	return s.addRun(taskID, run, status)
}

func (s *Store) addRun(taskID string, run AgentRun, status *Status) error {
	unlock, err := s.lockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(taskID)
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

// RunPatch describes a partial update to an AgentRun. Every field is a
// pointer: nil means "leave unchanged". Fields that carried an implicit
// non-empty/true guard in the old map[string]any path keep that guard here
// (see applyRunLifecycle/applyRunVerdict/applyRunTestOutcome/applyRunIdentity):
// HeadSHA and string verdict/test/session values ignore empty strings, and
// VerdictRendered is a latch that only ever flips true.
type RunPatch struct {
	// Lifecycle
	State   *string
	Result  *string
	LogFile *string
	HeadSHA *string

	// Cost/tokens
	CostUSD         *float64
	PremiumRequests *float64

	// Verdict
	Verdict         *string
	VerdictRendered *bool

	// Test outcome
	TestOutcome            *string
	TestFailureFingerprint *string
	ProtocolViolation      *string

	// Identity
	Provider        *string
	Model           *string
	ExperimentID    *string
	VariantID       *string
	AssignmentUnit  *string
	AssignmentKey   *string
	ReasoningEffort *string
	SessionID       *string
}

func applyRunLifecycle(run *AgentRun, p RunPatch) {
	if p.State != nil {
		run.State = *p.State
	}
	if p.Result != nil {
		run.Result = *p.Result
	}
	if p.LogFile != nil {
		run.LogFile = *p.LogFile
	}
	if p.HeadSHA != nil && *p.HeadSHA != "" {
		run.HeadSHA = *p.HeadSHA
	}
}

func applyRunCostTokens(run *AgentRun, p RunPatch) {
	if p.CostUSD != nil {
		run.CostUSD = *p.CostUSD
	}
	if p.PremiumRequests != nil {
		run.PremiumRequests = *p.PremiumRequests
	}
}

func applyRunVerdict(run *AgentRun, p RunPatch) {
	if p.Verdict != nil && *p.Verdict != "" {
		run.Verdict = *p.Verdict
	}
	if p.VerdictRendered != nil && *p.VerdictRendered {
		run.VerdictRendered = true
	}
}

func applyRunTestOutcome(run *AgentRun, p RunPatch) {
	if p.TestOutcome != nil && *p.TestOutcome != "" {
		run.TestOutcome = *p.TestOutcome
	}
	if p.TestFailureFingerprint != nil && *p.TestFailureFingerprint != "" {
		run.TestFailureFingerprint = *p.TestFailureFingerprint
	}
	if p.ProtocolViolation != nil && *p.ProtocolViolation != "" {
		run.ProtocolViolation = *p.ProtocolViolation
	}
}

func applyRunIdentity(run *AgentRun, p RunPatch) {
	if p.Provider != nil {
		run.Provider = *p.Provider
	}
	if p.Model != nil {
		run.Model = *p.Model
	}
	if p.ExperimentID != nil {
		run.ExperimentID = *p.ExperimentID
	}
	if p.VariantID != nil {
		run.VariantID = *p.VariantID
	}
	if p.AssignmentUnit != nil {
		run.AssignmentUnit = *p.AssignmentUnit
	}
	if p.AssignmentKey != nil {
		run.AssignmentKey = *p.AssignmentKey
	}
	if p.ReasoningEffort != nil {
		run.ReasoningEffort = *p.ReasoningEffort
	}
	if p.SessionID != nil && *p.SessionID != "" {
		run.SessionID = *p.SessionID
	}
}

func applyRunPatch(run *AgentRun, p RunPatch) {
	applyRunLifecycle(run, p)
	applyRunCostTokens(run, p)
	applyRunVerdict(run, p)
	applyRunTestOutcome(run, p)
	applyRunIdentity(run, p)
}

// UpdateRun applies patch to the AgentRun matching agentID within taskID's
// AgentRuns. Returns an error if the task or the run within it is not
// found.
func (s *Store) UpdateRun(taskID, agentID string, patch RunPatch) error {
	unlock, err := s.lockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(taskID)
	if err != nil {
		return err
	}
	found := false
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID != agentID {
			continue
		}
		found = true
		applyRunPatch(&t.AgentRuns[i], patch)
		break
	}
	if !found {
		return fmt.Errorf("agent run %s not found for task %s", agentID, taskID)
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
