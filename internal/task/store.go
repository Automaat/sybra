package task

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/reject"
	"github.com/google/uuid"
)

// Store is the filesystem-backed CRUD layer for task markdown files under
// dir (tasks/<id>.md). It also owns the sidecar stores for planning,
// review, and comment content that lives in adjacent files rather than the
// task's own frontmatter+body. Safe for concurrent use within a process; see
// lockTask for the cross-process locking story.
type Store struct {
	dir                 string
	trashDir            string
	comments            *CommentStore
	plans               *PlanningSidecarStore
	planContracts       *PlanningSidecarStore
	planDrafts          *PlanDraftStore
	planCritiques       *PlanningSidecarStore
	planResearch        *PlanningSidecarStore
	planDecisions       *PlanningSidecarStore
	planBrief           *PlanningSidecarStore
	codeReviews         *PlanningSidecarStore
	locker              *fsutil.KeyedLocker
	cacheMu             sync.RWMutex
	listCache           []Task
	listValid           bool
	listSnapshot        map[string]listFileState
	newTaskID           func() string
	refreshBeforeLock   func()
	currentTestFailures *PlanningSidecarStore
	acceptanceLedgers   *PlanningSidecarStore
	specDecisions       *PlanningSidecarStore
}

const maxTaskIDAttempts = 16
const taskLockTimeout = 2 * time.Second

// NewStore creates dir if it does not exist and returns a Store rooted
// there, along with its sidecar stores (comments, plans, code reviews).
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tasks dir: %w", err)
	}
	return &Store{
		dir:                 dir,
		trashDir:            filepath.Join(filepath.Dir(dir), "trash"),
		comments:            NewCommentStore(dir),
		plans:               NewPlanningSidecarStore(dir, ".plan.md", "plan"),
		planContracts:       NewPlanningSidecarStore(dir, ".plan-contract.json", "plan contract"),
		planDrafts:          NewPlanDraftStore(dir),
		planCritiques:       NewPlanningSidecarStore(dir, ".plan-critique.md", "plan critique"),
		planResearch:        NewPlanningSidecarStore(dir, ".plan-research.md", "plan research"),
		planDecisions:       NewPlanningSidecarStore(dir, ".plan-decisions.md", "plan decisions"),
		planBrief:           NewPlanningSidecarStore(dir, ".plan-brief.md", "plan brief"),
		codeReviews:         NewPlanningSidecarStore(dir, ".review.md", "code review"),
		locker:              fsutil.NewKeyedLocker(),
		newTaskID:           func() string { return uuid.NewString()[:8] },
		currentTestFailures: NewPlanningSidecarStore(dir, ".current-test-failures.md", "current test failures"),
		acceptanceLedgers:   NewPlanningSidecarStore(dir, ".acceptance-ledger.md", "acceptance ledger"),
		specDecisions:       NewPlanningSidecarStore(dir, ".spec-decision.md", "spec decision"),
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

// Plans returns the sidecar store for the human-readable compact plan
// (Task.Plan).
func (s *Store) Plans() *PlanningSidecarStore {
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
func (s *Store) PlanCritiques() *PlanningSidecarStore {
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
func (s *Store) CodeReviews() *PlanningSidecarStore {
	return s.codeReviews
}

// CurrentTestFailures returns the sidecar store for the latest bounded test
// failure report.
func (s *Store) CurrentTestFailures() *PlanningSidecarStore {
	return s.currentTestFailures
}

// AcceptanceLedgers returns the sidecar store for the bounded acceptance
// failure ledger.
func (s *Store) AcceptanceLedgers() *PlanningSidecarStore {
	return s.acceptanceLedgers
}

// SpecDecisions returns the sidecar store for the latest spec-decision
// escalation summary.
func (s *Store) SpecDecisions() *PlanningSidecarStore {
	return s.specDecisions
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
	path, err := s.safePath(id)
	if err != nil {
		return nil, err
	}
	unlock, err := s.locker.LockWithin(id, path, taskLockTimeout)
	if err != nil {
		return nil, fmt.Errorf("lock task %s: %w", id, err)
	}
	return unlock, nil
}

// lockNewTask serializes candidate-ID reservation without creating a visible
// task-dir entry. Ordinary task locks deliberately sit beside their task file,
// but creation has no file yet and must not leave a spurious .md.lock artifact
// that callers can mistake for task data.
func (s *Store) lockNewTask(id string) (func(), error) {
	// Resolve aliases first: two Store instances may address the same task
	// directory through a real path and a symlink, and must still contend on
	// one candidate-ID lock.
	taskDir, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return nil, fmt.Errorf("resolve task dir for creation lock: %w", err)
	}
	dir := filepath.Join(filepath.Dir(taskDir), ".task-create-locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create task lock dir: %w", err)
	}
	unlock, err := s.locker.LockWithin("create:"+id, filepath.Join(dir, id), taskLockTimeout)
	if err != nil {
		return nil, fmt.Errorf("lock new task %s: %w", id, err)
	}
	return unlock, nil
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
	".current-test-failures.md",
	".acceptance-ledger.md",
	".spec-decision.md",
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
// A task with a parse error is represented as a non-dispatchable, degraded
// Human Required entry rather than silently disappearing from the board.
// Results are served from an in-memory cache invalidated on any
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
	startSnapshot, hasStartSnapshot := s.listSnapshotFromEntries(entries)

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
			slog.Default().Warn("task.parse.degraded", "file", filepath.Base(p), "err", err)
			tasks = append(tasks, degradedTask(p, err))
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
		t.CurrentTestFailures = sidecars.currentTestFailures[t.ID]
		t.AcceptanceLedger = sidecars.acceptanceLedgers[t.ID]
		t.SpecDecision = sidecars.specDecisions[t.ID]
		if drafts, ok := sidecars.drafts[t.ID]; ok {
			t.PlanDrafts = drafts
		} else {
			t.PlanDrafts = map[string]string{}
		}
		migrateLegacyTask(&t, p)
		tasks = append(tasks, t)
	}
	if !parseErr {
		if hasStartSnapshot {
			s.storeListCacheIfSnapshotFresh(tasks, startSnapshot)
		} else {
			s.invalidateListCache()
		}
	}
	return tasks, nil
}

// degradedIDPrefix namespaces Store.List's synthetic entries for task files
// that could not be parsed. The colon is deliberately outside the character
// set ValidateID accepts, so a synthetic ID can never be minted, persisted,
// or collide with a real task ID; safePath additionally refuses to resolve
// one to a file, which is what keeps the degraded card read-only by
// construction rather than by convention.
const degradedIDPrefix = "unreadable:"

// IsDegradedID reports whether id is one of Store.List's synthetic
// unreadable-task identifiers rather than a real, addressable task ID.
func IsDegradedID(id string) bool {
	return strings.HasPrefix(id, degradedIDPrefix)
}

// degradedTask exposes an unreadable task file without trusting any of its
// frontmatter. The generated ID is deterministic for its filename and
// unaddressable by construction (see degradedIDPrefix), so the entry is
// read-only and can never be confused with a real task.
func degradedTask(path string, parseErr error) Task {
	base := filepath.Base(path)
	sum := sha256.Sum256([]byte(base))
	id := fmt.Sprintf("%s%x", degradedIDPrefix, sum[:8])
	modified := time.Time{}
	if info, err := os.Stat(path); err == nil {
		modified = info.ModTime().UTC()
	}
	reason := fmt.Sprintf("Task file %q cannot be parsed; repair it on disk to restore the task.", base)
	return Task{
		ID:              id,
		Slug:            "unreadable-task",
		Title:           "Unreadable task file: " + base,
		Status:          StatusHumanRequired,
		AgentMode:       AgentModeHeadless,
		StatusReason:    reason,
		Escalation:      autonomy.LegacyReason(reason),
		Body:            reason,
		CreatedAt:       modified,
		UpdatedAt:       modified,
		StatusChangedAt: modified,
		FilePath:        path,
		Degraded:        true,
		ParseError:      parseErr.Error(),
	}
}

// sidecarIndex holds sidecar contents loaded in a single ReadDir pass,
// indexed by task ID. Used by List to amortize sidecar I/O.
type sidecarIndex struct {
	plans               map[string]string
	contracts           map[string]string
	critiques           map[string]string
	research            map[string]string
	decisions           map[string]string
	briefs              map[string]string
	reviews             map[string]string
	currentTestFailures map[string]string
	acceptanceLedgers   map[string]string
	specDecisions       map[string]string
	drafts              map[string]map[string]string
}

type sidecarSpec struct {
	suffix string
	assign func(*sidecarIndex, string, string)
}

var sidecarSpecs = []sidecarSpec{
	{suffix: ".plan-critique.md", assign: func(idx *sidecarIndex, id, text string) { idx.critiques[id] = text }},
	{suffix: ".plan-contract.json", assign: func(idx *sidecarIndex, id, text string) { idx.contracts[id] = text }},
	{suffix: ".plan-research.md", assign: func(idx *sidecarIndex, id, text string) { idx.research[id] = text }},
	{suffix: ".plan-decisions.md", assign: func(idx *sidecarIndex, id, text string) { idx.decisions[id] = text }},
	{suffix: ".plan-brief.md", assign: func(idx *sidecarIndex, id, text string) { idx.briefs[id] = text }},
	{suffix: ".plan.md", assign: func(idx *sidecarIndex, id, text string) { idx.plans[id] = text }},
	{suffix: ".review.md", assign: func(idx *sidecarIndex, id, text string) { idx.reviews[id] = text }},
	{suffix: ".current-test-failures.md", assign: func(idx *sidecarIndex, id, text string) { idx.currentTestFailures[id] = text }},
	{suffix: ".acceptance-ledger.md", assign: func(idx *sidecarIndex, id, text string) { idx.acceptanceLedgers[id] = text }},
	{suffix: ".spec-decision.md", assign: func(idx *sidecarIndex, id, text string) { idx.specDecisions[id] = text }},
}

// loadSidecarsFromEntries reads sidecar contents for every recognized
// suffix in a single pass. Read failures on individual sidecars are
// logged and skipped — matches the prior `_ = err` behavior of the
// per-task sidecar Reads in List, where a corrupt sidecar should not
// abort the whole task list.
func loadSidecarsFromEntries(dir string, entries []os.DirEntry) *sidecarIndex {
	idx := &sidecarIndex{
		plans:               map[string]string{},
		contracts:           map[string]string{},
		critiques:           map[string]string{},
		research:            map[string]string{},
		decisions:           map[string]string{},
		briefs:              map[string]string{},
		reviews:             map[string]string{},
		currentTestFailures: map[string]string{},
		acceptanceLedgers:   map[string]string{},
		specDecisions:       map[string]string{},
		drafts:              map[string]map[string]string{},
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		if !strings.HasSuffix(base, ".md") && !strings.HasSuffix(base, ".json") {
			continue
		}
		if loadPlanDraftSidecar(dir, base, idx) {
			continue
		}
		loadIndexedSidecar(dir, base, idx)
	}
	return idx
}

func loadPlanDraftSidecar(dir, base string, idx *sidecarIndex) bool {
	if !IsPlanDraftFile(base) {
		return false
	}
	// IsPlanDraftFile already guarantees the prefix is present,
	// but using Cut + the found flag keeps the lint clean and is
	// resilient if the helper's contract loosens later.
	id, rest, found := strings.Cut(base, PlanDraftSidecarPrefix)
	if !found {
		return true
	}
	text, ok := readOptionalSidecarFile(dir, base)
	if !ok {
		return true
	}
	name := strings.TrimSuffix(rest, ".md")
	if idx.drafts[id] == nil {
		idx.drafts[id] = map[string]string{}
	}
	idx.drafts[id][name] = text
	return true
}

func loadIndexedSidecar(dir, base string, idx *sidecarIndex) bool {
	for _, spec := range sidecarSpecs {
		if !strings.HasSuffix(base, spec.suffix) {
			continue
		}
		text, ok := readOptionalSidecarFile(dir, base)
		if !ok {
			return true
		}
		spec.assign(idx, strings.TrimSuffix(base, spec.suffix), text)
		return true
	}
	return false
}

func readOptionalSidecarFile(dir, base string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, base))
	if err != nil {
		slog.Default().Warn("task.sidecar.read.skip", "file", base, "err", err)
		return "", false
	}
	return string(data), true
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
	if err := s.loadSidecars(&t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// loadSidecars populates t's planning/review sidecar fields from disk. Each
// sidecar store's Read/List already turns "sidecar absent" into a nil error
// with a zero value, so any error still returned here is a genuine read
// failure (e.g. a transient EIO) — propagate it instead of discarding it,
// since silently treating it as "no plan/review exists" would erase real
// content from the engine's view of the task.
func (s *Store) loadSidecars(t *Task) error {
	var err error
	if t.Plan, err = s.plans.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanContract, err = s.planContracts.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanCritique, err = s.planCritiques.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanResearch, err = s.planResearch.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanDecisions, err = s.planDecisions.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanBrief, err = s.planBrief.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.CodeReview, err = s.codeReviews.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.CurrentTestFailures, err = s.currentTestFailures.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.AcceptanceLedger, err = s.acceptanceLedgers.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.SpecDecision, err = s.specDecisions.Read(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	if t.PlanDrafts, err = s.planDrafts.List(t.ID); err != nil {
		return fmt.Errorf("load sidecars for %s: %w", t.ID, err)
	}
	return nil
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
	if IsDegradedID(id) {
		// Synthetic List-only entry for an unparseable file: it has no task
		// file of its own, and resolving it would let an update/delete issued
		// against the degraded board card land on some unrelated real task.
		return "", reject.New("task ID %q is a synthetic unreadable-file entry and has no task file", id)
	}
	path := filepath.Clean(filepath.Join(s.dir, id+".md"))
	if !strings.HasPrefix(path, filepath.Clean(s.dir)+string(filepath.Separator)) {
		return "", reject.New("invalid task ID %q", id)
	}
	return path, nil
}

// Create writes a new task file with a fresh ID, status "todo", and type
// "normal". Creation never replaces an existing task: a generated ID that
// already exists is retried. mode defaults to AgentModeHeadless when empty and is
// validated via ValidateMintableAgentMode. Use CreateFull to set additional
// fields (tags, project, priority, ...) atomically at creation time.
func (s *Store) Create(title, body, mode string) (Task, error) {
	if mode == "" {
		mode = AgentModeHeadless
	}
	if _, err := ValidateMintableAgentMode(mode); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	t := Task{
		Slug:            Slugify(title),
		Title:           title,
		Status:          StatusTodo,
		Generation:      1,
		AgentMode:       mode,
		Attachments:     []Attachment{},
		CreatedAt:       now,
		UpdatedAt:       now,
		StatusChangedAt: now,
		Body:            body,
	}

	if err := s.createNewTask(&t, nil); err != nil {
		return Task{}, err
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
		mode = AgentModeHeadless
	}
	if _, err := ValidateMintableAgentMode(mode); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	t := Task{
		Slug:            Slugify(title),
		Title:           title,
		Status:          StatusTodo,
		Generation:      1,
		AgentMode:       mode,
		Attachments:     []Attachment{},
		CreatedAt:       now,
		UpdatedAt:       now,
		StatusChangedAt: now,
		Body:            body,
	}
	// Apply initial field overrides before the first disk write so that any
	// watcher reading the file sees the complete task from the start.
	applyCreateInit(&t, init, now)
	if err := validateExplicitClear(init); err != nil {
		return Task{}, err
	}
	if err := blocker.ValidateStatus(string(t.Status), t.Blocker); err != nil {
		return Task{}, err
	}
	if err := normalizeSandboxEscapeHatch(&t); err != nil {
		return Task{}, err
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.Blocker)
	if err := s.createNewTask(&t, func() error {
		return s.writeSidecars(t.ID, init, &t)
	}); err != nil {
		return Task{}, err
	}
	s.storeTaskCache(t)
	return t, nil
}

// createNewTask locks a candidate ID, invokes beforePublish while no task file
// is visible, then exclusively publishes the primary task file. The lock spans
// the whole operation so a failed sidecar write can be rolled back before
// another current Store process can reuse the candidate ID. AtomicWriteNew
// independently refuses to replace an already-published primary task file.
func (s *Store) createNewTask(t *Task, beforePublish func() error) error {
	for range maxTaskIDAttempts {
		t.ID = s.newTaskID()
		t.FilePath = filepath.Join(s.dir, t.ID+".md")
		unlock, err := s.lockNewTask(t.ID)
		if err != nil {
			return err
		}
		if _, err := os.Stat(t.FilePath); err == nil {
			unlock()
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			unlock()
			return fmt.Errorf("stat task file: %w", err)
		}
		files, err := s.taskFiles(t.ID)
		if err != nil {
			unlock()
			return err
		}
		if len(files) > 0 {
			// A prior interrupted CreateFull may have left sidecars without
			// its primary file. Never bind that history to a fresh task.
			unlock()
			continue
		}
		if beforePublish != nil {
			if err := beforePublish(); err != nil {
				s.removeNewTaskSidecars(t.ID)
				unlock()
				return err
			}
		}
		data, err := Marshal(*t)
		if err != nil {
			s.removeNewTaskSidecars(t.ID)
			unlock()
			return err
		}
		writeErr := fsutil.AtomicWriteNew(t.FilePath, data)
		if writeErr == nil {
			unlock()
			return nil
		}
		// A writer outside Store raced our protected reservation. Do not
		// delete the sidecars: they may now belong to that writer's task.
		unlock()
		if errors.Is(writeErr, os.ErrExist) {
			continue
		}
		return fmt.Errorf("write task file: %w", writeErr)
	}
	return fmt.Errorf("create task: generated ID collided %d times", maxTaskIDAttempts)
}

// removeNewTaskSidecars rolls back sidecars written before task publication.
// createNewTask verifies this ID has no task or sidecars while holding the
// current Store's cross-process lock, so every sidecar found here belongs to
// this failed creation attempt. It deliberately never removes the primary
// task file.
func (s *Store) removeNewTaskSidecars(id string) {
	if files, err := s.taskFiles(id); err == nil {
		for _, name := range files {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
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
	if init.Blocker != nil {
		t.Blocker = *init.Blocker
	}
	if init.Escalation != nil {
		t.Escalation = *init.Escalation
	}
	if init.AutonomyOutcome != nil {
		t.AutonomyOutcome = *init.AutonomyOutcome
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
	if init.SandboxOffReason != nil {
		t.SandboxOffReason = *init.SandboxOffReason
	}
	if init.Sandbox != nil {
		t.Sandbox = init.Sandbox
	}
	if init.ReasoningEffort != nil {
		t.ReasoningEffort = *init.ReasoningEffort
	}
}

func validateExplicitClear(u Update) error {
	if u.ClearStatusReason != nil && *u.ClearStatusReason && u.StatusReason != nil {
		return reject.New("task update: status_reason and clear_status_reason cannot both be set")
	}
	if u.ClearBlocker != nil && *u.ClearBlocker && u.Blocker != nil {
		return reject.New("task update: blocker and clear_blocker cannot both be set")
	}
	return nil
}

// Put writes a fully-formed task to the store verbatim (upsert by ID),
// preserving the caller-supplied ID, status, workflow, and timestamps rather
// than minting new ones. It is the leader-follower execution mirror's write
// path (TaskService.AssignTask): the leader owns the canonical task and pushes
// it here, and the follower's file watcher then dispatches it through the
// normal workflow. Marshal persists frontmatter + body; sidecars are not
// written by this path.
// Put is a blind, verbatim upsert used by the cluster leader/follower mirror
// — most fields are trusted from the caller with no merge logic. Its #2203
// stale-status guard reads the existing on-disk task under s.lockTask, so
// concurrent Puts for the same ID are race-safe on their own; go through
// Manager.Put instead when the caller also needs Manager's lifecycle side
// effects (status-change hooks, created/updated events).
func (s *Store) Put(t Task) (Task, error) {
	if err := ValidateID(t.ID); err != nil {
		return Task{}, err
	}
	unlock, err := s.lockTask(t.ID)
	if err != nil {
		return Task{}, err
	}
	defer unlock()
	return s.putLocked(t)
}

// PutFn reads an existing task and writes the fully-formed replacement
// returned by fn while holding the task's cross-process write lock. It is for
// callers that need to merge a long-running operation's result with the most
// recent canonical task without a read-modify-write gap.
func (s *Store) PutFn(id string, fn func(cur Task) (Task, error)) (saved, previous Task, err error) {
	if err := ValidateID(id); err != nil {
		return Task{}, Task{}, err
	}
	unlock, err := s.lockTask(id)
	if err != nil {
		return Task{}, Task{}, err
	}
	defer unlock()

	cur, err := s.read(id)
	if err != nil {
		return Task{}, cur, err
	}
	next, err := fn(cur)
	if err != nil {
		return Task{}, cur, err
	}
	if next.ID != id {
		return Task{}, cur, fmt.Errorf("task: put-fn: callback changed task ID from %q to %q", id, next.ID)
	}
	saved, err = s.putLocked(next)
	return saved, cur, err
}

// putLocked is Put after the caller has acquired lockTask(t.ID).
func (s *Store) putLocked(t Task) (Task, error) {
	if err := validateWriteTask(t); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	// A status change whose UpdatedAt doesn't strictly advance past what's
	// already on disk is a stale snapshot, not a real update (#2203).
	// Fabricating a fresh timestamp on top of it would let the stale status
	// masquerade as the latest legitimate state to a consumer like the
	// cluster mirror's Merge — discard it instead and keep what's on disk.
	//
	// A mirror-applied task (MirrorUpdatedAt set, only by clusterlead.Merge)
	// proves freshness via MirrorRev instead: Merge runs fully serialized,
	// so an incoming MirrorRev past the on-disk value is race-free proof
	// even if an unrelated edit bumped UpdatedAt in the gap between Merge's
	// snapshot and this write reaching the lock above.
	existing, existingErr := s.read(t.ID)
	switch {
	case existingErr != nil:
		if !errors.Is(existingErr, os.ErrNotExist) {
			slog.Default().Warn("task.store.put.read_existing_failed", "id", t.ID, "err", existingErr)
		}
	case existing.Status != t.Status:
		if t.MirrorUpdatedAt != nil && t.MirrorRev > existing.MirrorRev {
			if !t.UpdatedAt.After(existing.UpdatedAt) {
				// now alone isn't guaranteed to advance past existing.UpdatedAt
				// (a prior genuinely-advancing Put can carry a caller-supplied
				// timestamp ahead of this process's wall clock) — fall back to
				// one tick past it so this write is never itself non-monotonic.
				t.UpdatedAt = existing.UpdatedAt.Add(time.Nanosecond)
				if now.After(t.UpdatedAt) {
					t.UpdatedAt = now
				}
			}
		} else if !t.UpdatedAt.After(existing.UpdatedAt) {
			t.Status = existing.Status
			t.Escalation = existing.Escalation
			t.AutonomyOutcome = existing.AutonomyOutcome
			t.UpdatedAt = existing.UpdatedAt
			t.StatusChangedAt = existing.StatusChangedAt
			// A rejected write's MirrorRev/MirrorUpdatedAt must not reach
			// disk either — otherwise a stale/duplicate push regresses the
			// mirror bookkeeping this same guard relies on to judge freshness
			// on the next mirror-authoritative Put for this task.
			t.MirrorRev = existing.MirrorRev
			t.MirrorUpdatedAt = existing.MirrorUpdatedAt
		}
	case t.Status == StatusHumanRequired && t.Escalation.IsZero() && !existing.Escalation.IsZero():
		t.Escalation = existing.Escalation
		t.AutonomyOutcome = existing.AutonomyOutcome
	}
	if t.Status == StatusHumanRequired {
		legacyContinuation := existingErr == nil &&
			existing.Status == StatusHumanRequired &&
			existing.Escalation.Provenance == autonomy.ProvenanceLegacy &&
			t.Escalation == existing.Escalation &&
			t.AutonomyOutcome == ""
		if !legacyContinuation {
			extra := Update{Escalation: &t.Escalation, AutonomyOutcome: &t.AutonomyOutcome}
			if err := validateHumanRequiredTransition(StatusTodo, t.Status, extra); err != nil {
				return Task{}, fmt.Errorf("task: put: %w", err)
			}
		}
	}
	if t.StatusChangedAt.IsZero() {
		t.StatusChangedAt = t.UpdatedAt
	}
	t.TaskType = normalizeTaskType(t.TaskType)
	data, err := marshalTask(t, false)
	if err != nil {
		return Task{}, err
	}
	t.FilePath = filepath.Join(s.dir, t.ID+".md")
	if err := fsutil.AtomicWrite(t.FilePath, data); err != nil {
		return Task{}, fmt.Errorf("write task file: %w", err)
	}
	s.storeTaskCache(t)
	return t, nil
}

// validateWriteTask enforces the values Parse rejects on every write path,
// including PutFn, which reaches putLocked directly while holding its task
// lock. Empty values retain the legacy compatibility accepted by Parse.
func validateWriteTask(t Task) error {
	if t.Slug != "" {
		if err := ValidateSlug(t.Slug); err != nil {
			return err
		}
	}
	if t.AgentMode != "" {
		if _, err := ValidateAgentMode(t.AgentMode); err != nil {
			return err
		}
	}
	return nil
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

// writeSidecars is invoked from CreateFull/UpdateWithPrev to persist Update
// fields that live in sidecar stores rather than the task frontmatter (plan,
// plan contract, plan critique, plan research/decisions/brief, code review).
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
	if u.CurrentTestFailures != nil {
		if err := s.currentTestFailures.Write(id, *u.CurrentTestFailures); err != nil {
			return fmt.Errorf("write current test failures: %w", err)
		}
		t.CurrentTestFailures = *u.CurrentTestFailures
	}
	if u.AcceptanceLedger != nil {
		if err := s.acceptanceLedgers.Write(id, *u.AcceptanceLedger); err != nil {
			return fmt.Errorf("write acceptance ledger: %w", err)
		}
		t.AcceptanceLedger = *u.AcceptanceLedger
	}
	if u.SpecDecision != nil {
		if err := s.specDecisions.Write(id, *u.SpecDecision); err != nil {
			return fmt.Errorf("write spec decision: %w", err)
		}
		t.SpecDecision = *u.SpecDecision
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
	if u.CodeReviewVerdict != nil {
		t.CodeReviewVerdict = *u.CodeReviewVerdict
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
	if u.ReviewedHeadSHA != nil {
		t.ReviewedHeadSHA = *u.ReviewedHeadSHA
	}
	if u.ReviewedHeadAttempts != nil {
		t.ReviewedHeadAttempts = *u.ReviewedHeadAttempts
	}
	if u.ReconcileFailures != nil {
		t.ReconcileFailures = *u.ReconcileFailures
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
	if u.DependsOnConditions != nil {
		t.DependsOnConditions = slices.Clone(*u.DependsOnConditions)
	}
}

// normalizeSandboxEscapeHatch keeps the escape hatch and its justification in
// one consistent state. Disabling the sandbox hands a task's agents
// unrestricted write access to the host, so it must be justified at the point
// an operator asks for it — an unexplained bypass is not something to discover
// later in the audit log.
//
// The reason is only meaningful while the hatch is actually off, so it is
// dropped otherwise rather than left behind as stale frontmatter. That makes
// the flip and its reason a single call: setting sandbox=false in one request
// and the reason in a later one is refused, not silently accepted half-done.
func normalizeSandboxEscapeHatch(t *Task) error {
	if t.Sandbox != nil && !*t.Sandbox {
		if strings.TrimSpace(t.SandboxOffReason) == "" {
			return reject.New("sandbox: disabling the sandbox requires sandbox_off_reason explaining why")
		}
		t.SandboxOffReason = strings.TrimSpace(t.SandboxOffReason)
		return nil
	}
	t.SandboxOffReason = ""
	return nil
}

//nolint:funlen // Centralized field application keeps persistence validation exhaustive.
func applyUpdateFields(t *Task, u Update) error {
	if err := validateExplicitClear(u); err != nil {
		return err
	}
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
		statusChanged := *u.Status != oldStatus
		if statusChanged && u.Escalation == nil && t.Status != StatusHumanRequired {
			t.Escalation = autonomy.EscalationReason{}
		}
		if statusChanged && u.AutonomyOutcome == nil {
			t.AutonomyOutcome = ""
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
	if u.ClearStatusReason != nil && *u.ClearStatusReason {
		t.StatusReason = ""
	}
	if u.StatusReason != nil {
		t.StatusReason = *u.StatusReason
	}
	if u.ClearBlocker != nil && *u.ClearBlocker {
		t.Blocker = blocker.State{}
	}
	if u.Escalation != nil {
		if err := u.Escalation.Validate(); err != nil {
			return reject.New("typed escalation: %w", err)
		}
		if u.AutonomyOutcome == nil || !u.AutonomyOutcome.IsKnown() {
			return reject.New("typed escalation requires a known autonomy outcome")
		}
		t.Escalation = *u.Escalation
	}
	if u.AutonomyOutcome != nil {
		t.AutonomyOutcome = *u.AutonomyOutcome
	}
	if u.Blocker != nil {
		if err := blocker.ValidateStatus(string(t.Status), *u.Blocker); err != nil {
			return err
		}
		t.Blocker = *u.Blocker
	}
	if u.BlockedByIssue != nil {
		t.BlockedByIssue = *u.BlockedByIssue
	}
	if u.SupervisorSteer != nil {
		t.SupervisorSteer = *u.SupervisorSteer
	}
	if u.AgentMode != nil {
		if _, err := ValidateMintableAgentMode(*u.AgentMode); err != nil {
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
	if u.Priority != nil {
		t.Priority = *u.Priority
	}
	if u.DueDate != nil {
		t.DueDate = *u.DueDate
	}
	if u.ClearWorkflow != nil && *u.ClearWorkflow {
		t.Workflow = nil
	} else if u.Workflow != nil {
		t.Workflow = *u.Workflow
	}
	if u.MaxTurns != nil {
		t.MaxTurns = *u.MaxTurns
	}
	if u.ForkSubagent != nil {
		t.ForkSubagent = *u.ForkSubagent
	}
	if u.SandboxOffReason != nil {
		t.SandboxOffReason = *u.SandboxOffReason
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
	if u.Attachments != nil {
		t.Attachments = slices.Clone(*u.Attachments)
	}
	if u.EffectLog != nil {
		t.EffectLog = slices.Clone(*u.EffectLog)
	}
	return normalizeSandboxEscapeHatch(t)
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
	statusChangedBackfill := statusChangedAtBackfill(t, now)
	t.Generation++
	t.UpdatedAt = now
	if t.Status != prevStatus {
		t.StatusChangedAt = now
	} else if t.StatusChangedAt.IsZero() {
		t.StatusChangedAt = statusChangedBackfill
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.Blocker)
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

// UpdateMap converts raw to a typed Update and applies it.
// Returns an error on unknown keys or wrong value types.
func (s *Store) UpdateMap(id string, raw map[string]any) (Task, error) {
	u, err := UpdateFromMap(raw)
	if err != nil {
		return Task{}, err
	}
	return s.Update(id, u)
}
