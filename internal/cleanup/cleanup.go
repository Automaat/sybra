// Package cleanup implements the scan/size/apply logic behind
// `sybra-cli doctor cleanup`: it reports reclaimable Sybra caches, logs,
// sandboxes, and worktrees, and (opt-in) deletes them.
//
// Safety spine: a resource is eligible for cleanup only when its owning task
// is absent from the store (orphan) or in a cleanupEligible status
// (done/cancelled/blocked) and has been for at least a retention window,
// mirroring the in-app sandbox sweep (internal/sandbox.cleanupEligible +
// task.StatusChangedAt). Scan reports exactly the set Apply would delete;
// Apply re-derives eligibility from a fresh task snapshot immediately before
// each delete, so a task that became active between Scan and Apply is never
// removed.
package cleanup

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/buildcache"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
)

// Risk classifies how dangerous it is to delete a bucket's contents.
type Risk string

const (
	RiskSafe        Risk = "safe"
	RiskCaution     Risk = "caution"
	RiskDestructive Risk = "destructive"
)

// Bucket names. Safe buckets are included in the default report/apply set;
// destructive buckets require their gate flag (see Options) to be included
// in either Scan or Apply.
const (
	BucketLogs         = "logs"
	BucketAudit        = "audit"
	BucketSandboxes    = "sandboxes"
	BucketGoBuildCache = "go-build-cache"
	BucketWorktrees    = "worktrees"
	BucketSharedCache  = "shared-cache"
	BucketExternal     = "external"
)

// unknownTaskID marks a resource whose owning task could not be determined
// from its directory name. Such resources are always reported as ineligible
// — they are never deleted, with or without --force.
const unknownTaskID = "unknown"

// AllBucketNames returns every bucket name Scan/Apply understand, in display order.
func AllBucketNames() []string {
	return []string{BucketLogs, BucketAudit, BucketSandboxes, BucketGoBuildCache, BucketWorktrees, BucketSharedCache, BucketExternal}
}

// ValidBucketName reports whether name is a known bucket.
func ValidBucketName(name string) bool {
	for _, n := range AllBucketNames() {
		if n == name {
			return true
		}
	}
	return false
}

// Bucket is one reclaimable category of disk usage: the set of paths Scan
// found eligible for deletion right now, sized and risk-tagged.
type Bucket struct {
	Name        string
	Risk        Risk
	Description string
	Paths       []string
	Bytes       int64
	Items       int
}

// Options mirrors the `doctor cleanup` CLI flags 1:1.
type Options struct {
	// Only restricts Scan/Apply to these bucket names. Empty means all
	// bucket names (still subject to the Worktrees/External/Force gates).
	Only []string
	// Worktrees gates inclusion of the destructive worktrees bucket.
	Worktrees bool
	// External gates inclusion of the report-only external (docker) bucket.
	External bool
	// Force gates inclusion of the destructive shared-cache bucket, and
	// bypasses the per-worktree dirty-git-status safety check.
	Force bool
	// OlderThan overrides the log/audit file age threshold. Zero means use
	// the configured retention default. Never affects sandbox/go-build-
	// cache/worktree eligibility, which is task-status driven.
	OlderThan time.Duration
}

// Skip records a path Apply declined to delete, and why.
type Skip struct {
	Path   string
	Reason string
}

// PathError records a path Apply tried to delete but failed.
type PathError struct {
	Path string
	Err  string
}

// BucketResult is Apply's outcome for one bucket.
type BucketResult struct {
	Name           string
	ReclaimedBytes int64
	Removed        int
	Skipped        []Skip
	Errors         []PathError
}

// Result is Scan's output: one Bucket per requested/eligible category.
type Result struct {
	Buckets []Bucket
}

// ApplyResult is Apply's output: one BucketResult per bucket that was
// selected for deletion.
type ApplyResult struct {
	Buckets []BucketResult
}

// TaskLister is the subset of *task.Manager Scanner needs. Scan and Apply
// each call List() fresh, so Apply always revalidates against current state.
type TaskLister interface {
	List() ([]task.Task, error)
}

// Scanner scans and applies cleanup over one Sybra home directory.
type Scanner struct {
	cfg   *config.Config
	tasks TaskLister
	// now is the injectable clock; defaults to time.Now.
	now func() time.Time
	// externalRunner is the injectable external-bucket probe; defaults to
	// shelling out to `docker system df`.
	externalRunner func() (string, error)
}

// NewScanner builds a Scanner over cfg's resolved directories and tasks
// (typically the CLI's live task.Manager, or a fake in tests).
func NewScanner(cfg *config.Config, tasks TaskLister) *Scanner {
	return &Scanner{cfg: cfg, tasks: tasks, now: time.Now}
}

type snapshot struct {
	byID map[string]task.Task
}

// snapshot reloads the task store. Called fresh by both Scan and Apply so
// Apply's revalidation never trusts a stale scan-time state.
func (s *Scanner) snapshot() (snapshot, error) {
	tasks, err := s.tasks.List()
	if err != nil {
		return snapshot{}, fmt.Errorf("list tasks: %w", err)
	}
	m := make(map[string]task.Task, len(tasks))
	for _, t := range tasks {
		m[t.ID] = t
	}
	return snapshot{byID: m}, nil
}

// cleanupEligible mirrors internal/sandbox.cleanupEligible: a task's
// resources are cleanup candidates once it is done/cancelled/blocked. This
// is deliberately broader than task.IsTerminalStatus (which omits blocked):
// a blocked task has no live agent and cannot resume without a status
// change, so its resources are just as safe to age out as a done task's.
func cleanupEligible(st task.Status) bool {
	return task.IsTerminalStatus(st) || st == task.StatusBlocked
}

// eligible reports whether taskID's resource at path may be cleaned up: true
// when the task is absent from snap (orphan), or cleanupEligible and past
// retention (measured from StatusChangedAt, falling back to path's mtime
// when StatusChangedAt is zero). retentionDisabled (config value < 0) always
// refuses age-based cleanup, deferring to task deletion instead. An empty or
// unknownTaskID is always ineligible — callers must resolve the owning task
// before calling this.
func eligible(snap snapshot, taskID, path string, retention time.Duration, retentionDisabled bool, now time.Time) (bool, string) {
	if taskID == "" || taskID == unknownTaskID {
		return false, "unknown owning task"
	}
	t, exists := snap.byID[taskID]
	if !exists {
		return true, "orphan (task not found)"
	}
	if !cleanupEligible(t.Status) {
		return false, fmt.Sprintf("task %s is active (status %s)", taskID, t.Status)
	}
	if retentionDisabled {
		return false, "age-based retention disabled"
	}
	staleSince := t.StatusChangedAt
	if staleSince.IsZero() {
		if info, err := os.Stat(path); err == nil {
			staleSince = info.ModTime()
		}
	}
	if staleSince.IsZero() || now.Sub(staleSince) < retention {
		return false, "within retention window"
	}
	return true, fmt.Sprintf("task %s past retention (status %s)", taskID, t.Status)
}

// dirSize sums the size of every regular file under path, including path
// itself if it is a plain file.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

var (
	bareTaskIDRe   = regexp.MustCompile(`^[0-9a-f]{8}$`)
	suffixTaskIDRe = regexp.MustCompile(`-([0-9a-f]{8})$`)
)

// taskIDFromWorktreeDir extracts the owning task ID from a worktree
// directory name: task.Task.DirName() is either the bare 8-hex-char ID (no
// slug yet) or "<slug>-<8hex-id>". Anything else (a human-created or
// externally-adopted directory) returns unknownTaskID — never deleted.
func taskIDFromWorktreeDir(name string) string {
	if bareTaskIDRe.MatchString(name) {
		return name
	}
	if m := suffixTaskIDRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return unknownTaskID
}

// containedChild reports whether path is a strict descendant of root (not
// root itself), rejecting any ".." escape. Returns the cleaned path.
func containedChild(root, path string) (string, bool) {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return cleanPath, true
}

// isSymlink reports whether path exists and is a symlink. A stat failure
// (including a missing path) is treated as "not a symlink" — callers already
// handle a missing path separately.
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

func sandboxesDir() string {
	return filepath.Join(config.HomeDir(), "sandboxes")
}

func goBuildCacheDir() string {
	return filepath.Join(buildcache.SharedRoot(), "go-build")
}

// gitStatusClean reports whether the worktree at path has no pending
// changes. A non-git directory or any git failure is treated as "not clean"
// — the safe default is to skip, not delete.
func gitStatusClean(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) == 0
}

// resolveBucketNames returns the bucket names Scan/Apply should consider:
// opts.Only when set (already validated by the caller), otherwise every
// known bucket name (still subject to each bucket's own gate check).
func resolveBucketNames(opts Options) []string {
	if len(opts.Only) > 0 {
		return opts.Only
	}
	return AllBucketNames()
}

// Scan reports, per requested bucket, exactly the paths currently eligible
// for deletion under opts — the set Apply would remove if called right now
// with the same opts. Destructive buckets (worktrees, shared-cache,
// external) are included only when their gate flag is set, independent of
// whether they were named in opts.Only.
func (s *Scanner) Scan(opts Options) (Result, error) {
	if err := validateOnly(opts.Only); err != nil {
		return Result{}, err
	}
	snap, err := s.snapshot()
	if err != nil {
		return Result{}, err
	}

	var buckets []Bucket
	for _, name := range resolveBucketNames(opts) {
		switch name {
		case BucketLogs:
			buckets = append(buckets, s.scanLogs(opts))
		case BucketAudit:
			buckets = append(buckets, s.scanAudit(opts))
		case BucketSandboxes:
			buckets = append(buckets, s.scanSandboxes(snap))
		case BucketGoBuildCache:
			buckets = append(buckets, s.scanGoBuildCache(snap))
		case BucketWorktrees:
			if !opts.Worktrees {
				continue
			}
			buckets = append(buckets, s.scanWorktrees(snap, opts))
		case BucketSharedCache:
			if !opts.Force {
				continue
			}
			buckets = append(buckets, s.scanSharedCache())
		case BucketExternal:
			if !opts.External {
				continue
			}
			buckets = append(buckets, s.scanExternal())
		}
	}
	return Result{Buckets: buckets}, nil
}

func validateOnly(only []string) error {
	for _, n := range only {
		if !ValidBucketName(n) {
			return fmt.Errorf("unknown bucket %q (valid: %s)", n, strings.Join(AllBucketNames(), ", "))
		}
	}
	return nil
}

func (s *Scanner) retentionDays(bucketName string, opts Options) (days int, disabled bool) {
	switch bucketName {
	case BucketLogs:
		days = s.cfg.DefaultLogRetentionDays()
	case BucketAudit:
		days = s.cfg.Audit.RetentionDays
		if days <= 0 {
			days = 30
		}
	}
	if opts.OlderThan > 0 {
		return int(opts.OlderThan.Hours() / 24), false
	}
	if days < 0 {
		return 0, true
	}
	return days, false
}

// ageEligible reports whether a plain file (logs/audit) is past its
// retention window. opts.OlderThan, when set, overrides the configured
// per-bucket default; a negative configured default (and no override)
// disables age-based deletion for that bucket entirely.
func (s *Scanner) ageEligible(bucketName, path string, opts Options) (bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		return false, "stat failed"
	}
	days, disabled := s.retentionDays(bucketName, opts)
	if disabled {
		return false, "age-based retention disabled"
	}
	retention := time.Duration(days) * 24 * time.Hour
	if opts.OlderThan > 0 {
		retention = opts.OlderThan
	}
	if s.now().Sub(info.ModTime()) < retention {
		return false, "within retention window"
	}
	return true, "past retention"
}

func (s *Scanner) scanLogs(opts Options) Bucket {
	dir := filepath.Join(s.cfg.Logging.Dir, "agents")
	b := Bucket{Name: BucketLogs, Risk: RiskSafe, Description: "per-agent NDJSON logs past retention"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return b
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isSymlink(p) {
			continue
		}
		if ok, _ := s.ageEligible(BucketLogs, p, opts); !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += info.Size()
		b.Items++
	}
	return b
}

func (s *Scanner) scanAudit(opts Options) Bucket {
	dir := s.cfg.AuditDir()
	b := Bucket{Name: BucketAudit, Risk: RiskSafe, Description: "daily audit NDJSON logs past retention"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return b
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isSymlink(p) {
			continue
		}
		if ok, _ := s.ageEligible(BucketAudit, p, opts); !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += info.Size()
		b.Items++
	}
	return b
}

func (s *Scanner) sandboxRetention() (time.Duration, bool) {
	return s.cfg.DefaultSandboxRetention()
}

func (s *Scanner) scanSandboxes(snap snapshot) Bucket {
	dir := sandboxesDir()
	b := Bucket{Name: BucketSandboxes, Risk: RiskSafe, Description: "per-task sandbox dirs for orphaned/terminal tasks"}
	retention, disabled := s.sandboxRetention()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return b
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isSymlink(p) {
			continue
		}
		if ok, _ := eligible(snap, e.Name(), p, retention, disabled, s.now()); !ok {
			continue
		}
		size, err := dirSize(p)
		if err != nil {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += size
		b.Items++
	}
	return b
}

func (s *Scanner) scanGoBuildCache(snap snapshot) Bucket {
	dir := goBuildCacheDir()
	b := Bucket{Name: BucketGoBuildCache, Risk: RiskSafe, Description: "orphaned/terminal-task per-task Go build cache dirs"}
	retention, disabled := s.sandboxRetention()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return b
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isSymlink(p) {
			continue
		}
		if ok, _ := eligible(snap, e.Name(), p, retention, disabled, s.now()); !ok {
			continue
		}
		size, err := dirSize(p)
		if err != nil {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += size
		b.Items++
	}
	return b
}

func (s *Scanner) scanWorktrees(snap snapshot, opts Options) Bucket {
	dir := s.cfg.WorktreesDir
	b := Bucket{Name: BucketWorktrees, Risk: RiskDestructive, Description: "per-task git worktrees for orphaned/terminal tasks (clean only, unless --force)"}
	retention, disabled := s.sandboxRetention()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return b
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isSymlink(p) {
			continue
		}
		taskID := taskIDFromWorktreeDir(e.Name())
		if ok, _ := eligible(snap, taskID, p, retention, disabled, s.now()); !ok {
			continue
		}
		if !opts.Force && !gitStatusClean(p) {
			continue
		}
		size, err := dirSize(p)
		if err != nil {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += size
		b.Items++
	}
	return b
}

func (s *Scanner) scanSharedCache() Bucket {
	b := Bucket{Name: BucketSharedCache, Risk: RiskDestructive, Description: "shared go-mod/npm caches, regenerable via mise install/npm ci; deleting affects every in-flight build"}
	for _, p := range []string{buildcache.SharedGoModDir(), buildcache.SharedNPMDir()} {
		if isSymlink(p) {
			continue
		}
		size, err := dirSize(p)
		if err != nil || size == 0 {
			continue
		}
		b.Paths = append(b.Paths, p)
		b.Bytes += size
		b.Items++
	}
	return b
}

func defaultExternalRunner() (string, error) {
	out, err := exec.Command("docker", "system", "df").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// scanExternal is report-only: it never returns real filesystem paths, and
// Apply refuses to touch this bucket unconditionally.
func (s *Scanner) scanExternal() Bucket {
	b := Bucket{Name: BucketExternal, Risk: RiskCaution, Description: "external docker resources (report-only; sybra-cli never deletes these)"}
	runner := s.externalRunner
	if runner == nil {
		runner = defaultExternalRunner
	}
	summary, err := runner()
	if err != nil || summary == "" {
		return b
	}
	b.Items = 1
	b.Description = b.Description + ": " + summary
	return b
}

// Apply deletes the paths in selected (normally Scan's own Result.Buckets)
// after reloading the task store and re-checking eligibility, containment,
// symlink-safety, and (for worktrees) git cleanliness immediately before
// each delete. A path that fails revalidation is skipped with a reason, not
// treated as an error; a delete that fails is recorded as an error and
// processing continues with the next path.
func (s *Scanner) Apply(selected []Bucket, opts Options) (ApplyResult, error) {
	snap, err := s.snapshot()
	if err != nil {
		return ApplyResult{}, err
	}

	var result ApplyResult
	for _, b := range selected {
		result.Buckets = append(result.Buckets, s.applyBucket(b, snap, opts))
	}
	return result, nil
}

func (s *Scanner) applyBucket(b Bucket, snap snapshot, opts Options) BucketResult {
	br := BucketResult{Name: b.Name}
	if b.Name == BucketExternal {
		for _, p := range b.Paths {
			br.Skipped = append(br.Skipped, Skip{Path: p, Reason: "external bucket is report-only"})
		}
		return br
	}
	for _, p := range b.Paths {
		ok, reason := s.revalidate(b.Name, p, snap, opts)
		if !ok {
			br.Skipped = append(br.Skipped, Skip{Path: p, Reason: reason})
			continue
		}
		size, _ := dirSize(p)
		if err := fsutil.RemoveAllForce(p); err != nil {
			br.Errors = append(br.Errors, PathError{Path: p, Err: err.Error()})
			continue
		}
		br.Removed++
		br.ReclaimedBytes += size
	}
	return br
}

func (s *Scanner) bucketRoot(bucketName string) (string, bool) {
	switch bucketName {
	case BucketLogs:
		return filepath.Join(s.cfg.Logging.Dir, "agents"), true
	case BucketAudit:
		return s.cfg.AuditDir(), true
	case BucketSandboxes:
		return sandboxesDir(), true
	case BucketGoBuildCache:
		return goBuildCacheDir(), true
	case BucketWorktrees:
		return s.cfg.WorktreesDir, true
	case BucketSharedCache:
		return buildcache.SharedRoot(), true
	default:
		return "", false
	}
}

// revalidate re-derives eligibility for path immediately before deletion,
// using a fresh snapshot rather than trusting Scan's (possibly stale)
// result.
func (s *Scanner) revalidate(bucketName, path string, snap snapshot, opts Options) (bool, string) {
	root, ok := s.bucketRoot(bucketName)
	if !ok {
		return false, "unknown bucket"
	}
	if _, ok := containedChild(root, path); !ok {
		return false, "path escapes bucket root"
	}
	if isSymlink(path) {
		return false, "refusing to follow symlink"
	}
	if _, err := os.Stat(path); err != nil {
		return false, "no longer exists"
	}

	switch bucketName {
	case BucketLogs, BucketAudit:
		return s.ageEligible(bucketName, path, opts)
	case BucketSandboxes, BucketGoBuildCache:
		retention, disabled := s.sandboxRetention()
		return eligible(snap, filepath.Base(path), path, retention, disabled, s.now())
	case BucketWorktrees:
		retention, disabled := s.sandboxRetention()
		taskID := taskIDFromWorktreeDir(filepath.Base(path))
		if ok, reason := eligible(snap, taskID, path, retention, disabled, s.now()); !ok {
			return false, reason
		}
		if !opts.Force && !gitStatusClean(path) {
			return false, "worktree has uncommitted changes"
		}
		return true, "eligible"
	case BucketSharedCache:
		return true, "shared cache"
	default:
		return false, "unhandled bucket"
	}
}
