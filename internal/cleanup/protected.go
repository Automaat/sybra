package cleanup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/task"
)

const (
	ReasonUnpushedCommits = "unpushed_commits"
	DefaultReminderWindow = 24 * time.Hour
)

type ResourceKind string

const (
	ResourceWorktree ResourceKind = "worktree"
	ResourceSandbox  ResourceKind = "sandbox"
)

type FindingState string

const (
	FindingOpen       FindingState = "open"
	FindingResolved   FindingState = "resolved"
	FindingDiscarded  FindingState = "discarded"
	FindingRescued    FindingState = "rescued"
	FindingReattached FindingState = "reattached"
)

type ObserveEvent string

const (
	ObserveUnchanged ObserveEvent = "unchanged"
	ObserveCreated   ObserveEvent = "created"
	ObserveChanged   ObserveEvent = "changed"
	ObserveReopened  ObserveEvent = "reopened"
	ObserveReminder  ObserveEvent = "reminder"
)

func (e ObserveEvent) ShouldLog() bool {
	return e == ObserveCreated || e == ObserveChanged || e == ObserveReopened || e == ObserveReminder
}

type Observation struct {
	Kind          ResourceKind `json:"kind"`
	TaskID        string       `json:"taskId,omitempty"`
	Path          string       `json:"path"`
	Reason        string       `json:"reason"`
	ObservedHead  string       `json:"observedHead,omitempty"`
	ObservedState string       `json:"observedState,omitempty"`
	BytesRetained int64        `json:"bytesRetained"`
}

type RescueInfo struct {
	Ref         string    `json:"ref,omitempty"`
	ArchivePath string    `json:"archivePath,omitempty"`
	VerifiedAt  time.Time `json:"verifiedAt,omitzero"`
}

type Finding struct {
	ID            string       `json:"id"`
	Kind          ResourceKind `json:"kind"`
	TaskID        string       `json:"taskId,omitempty"`
	LinkedTaskID  string       `json:"linkedTaskId,omitempty"`
	Path          string       `json:"path"`
	Reason        string       `json:"reason"`
	ObservedHead  string       `json:"observedHead,omitempty"`
	ObservedState string       `json:"observedState,omitempty"`
	BytesRetained int64        `json:"bytesRetained"`
	State         FindingState `json:"state"`
	FirstSeenAt   time.Time    `json:"firstSeenAt"`
	LastSeenAt    time.Time    `json:"lastSeenAt"`
	LastChangedAt time.Time    `json:"lastChangedAt"`
	LastLoggedAt  time.Time    `json:"lastLoggedAt"`
	ResolvedAt    time.Time    `json:"resolvedAt,omitzero"`
	Rescue        RescueInfo   `json:"rescue,omitzero"`
}

func (f Finding) EvidenceTaskID() string {
	if strings.TrimSpace(f.LinkedTaskID) != "" {
		return f.LinkedTaskID
	}
	return f.TaskID
}

type protectedFile struct {
	Findings []Finding `json:"findings"`
}

var protectedStoreLocker = fsutil.NewKeyedLocker()

type ProtectedStore struct {
	path           string
	reminderWindow time.Duration
	now            func() time.Time
}

func DefaultProtectedStorePath() string {
	return filepath.Join(config.HomeDir(), "cleanup", "protected-findings.json")
}

func DefaultProtectedStore() *ProtectedStore {
	return NewProtectedStore(DefaultProtectedStorePath())
}

func NewProtectedStore(path string) *ProtectedStore {
	return &ProtectedStore{
		path:           path,
		reminderWindow: DefaultReminderWindow,
		now:            time.Now,
	}
}

func (s *ProtectedStore) lock() (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	path := strings.TrimSpace(s.path)
	if path == "" {
		return func() {}, nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	unlock, err := protectedStoreLocker.Lock(path, path)
	if err != nil {
		return nil, err
	}
	return unlock, nil
}

func (s *ProtectedStore) read() (protectedFile, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return protectedFile{}, nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return protectedFile{}, nil
	}
	if err != nil {
		return protectedFile{}, err
	}
	var rec protectedFile
	if err := json.Unmarshal(data, &rec); err != nil {
		return protectedFile{}, err
	}
	return rec, nil
}

func (s *ProtectedStore) write(rec protectedFile) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	sort.Slice(rec.Findings, func(i, j int) bool {
		return rec.Findings[i].ID < rec.Findings[j].ID
	})
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, data)
}

func (s *ProtectedStore) List() ([]Finding, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	rec, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Finding, len(rec.Findings))
	copy(out, rec.Findings)
	return out, nil
}

func (s *ProtectedStore) Get(id string) (Finding, bool, error) {
	unlock, err := s.lock()
	if err != nil {
		return Finding{}, false, err
	}
	defer unlock()

	rec, err := s.read()
	if err != nil {
		return Finding{}, false, err
	}
	for i := range rec.Findings {
		if rec.Findings[i].ID == id {
			return rec.Findings[i], true, nil
		}
	}
	return Finding{}, false, nil
}

func (s *ProtectedStore) Observe(obs Observation) (Finding, ObserveEvent, error) {
	unlock, err := s.lock()
	if err != nil {
		return Finding{}, ObserveUnchanged, err
	}
	defer unlock()

	now := s.now().UTC()
	rec, err := s.read()
	if err != nil {
		return Finding{}, ObserveUnchanged, err
	}
	id := protectedFindingID(obs.Kind, obs.Path, obs.Reason)
	idx := -1
	for i := range rec.Findings {
		if rec.Findings[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		f := Finding{
			ID:            id,
			Kind:          obs.Kind,
			TaskID:        obs.TaskID,
			Path:          obs.Path,
			Reason:        obs.Reason,
			ObservedHead:  obs.ObservedHead,
			ObservedState: obs.ObservedState,
			BytesRetained: obs.BytesRetained,
			State:         FindingOpen,
			FirstSeenAt:   now,
			LastSeenAt:    now,
			LastChangedAt: now,
			LastLoggedAt:  now,
		}
		rec.Findings = append(rec.Findings, f)
		if err := s.write(rec); err != nil {
			return Finding{}, ObserveUnchanged, err
		}
		return f, ObserveCreated, nil
	}

	f := rec.Findings[idx]
	changed := f.ObservedHead != obs.ObservedHead ||
		f.ObservedState != obs.ObservedState ||
		f.BytesRetained != obs.BytesRetained ||
		f.TaskID != obs.TaskID ||
		f.Path != obs.Path

	f.Kind = obs.Kind
	f.TaskID = obs.TaskID
	f.Path = obs.Path
	f.Reason = obs.Reason
	f.ObservedHead = obs.ObservedHead
	f.ObservedState = obs.ObservedState
	f.BytesRetained = obs.BytesRetained
	f.LastSeenAt = now

	event := ObserveUnchanged
	switch f.State {
	case FindingOpen:
		if changed {
			f.LastChangedAt = now
			f.LastLoggedAt = now
			event = ObserveChanged
		} else if f.LastLoggedAt.IsZero() || now.Sub(f.LastLoggedAt) >= s.reminderWindow {
			f.LastLoggedAt = now
			event = ObserveReminder
		}
	default:
		if changed {
			f.State = FindingOpen
			f.LastChangedAt = now
			f.LastLoggedAt = now
			f.ResolvedAt = time.Time{}
			f.Rescue = RescueInfo{}
			event = ObserveReopened
		}
	}

	rec.Findings[idx] = f
	if err := s.write(rec); err != nil {
		return Finding{}, ObserveUnchanged, err
	}
	return f, event, nil
}

func (s *ProtectedStore) ResolveMissing(kind ResourceKind, observed map[string]bool) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	rec, err := s.read()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	changed := false
	for i := range rec.Findings {
		f := &rec.Findings[i]
		if f.Kind != kind || f.State != FindingOpen {
			continue
		}
		if observed[f.ID] {
			continue
		}
		f.State = FindingResolved
		f.ResolvedAt = now
		changed = true
	}
	if !changed {
		return nil
	}
	return s.write(rec)
}

func (s *ProtectedStore) Discard(id string) (Finding, error) {
	return s.setState(id, FindingDiscarded, func(_ *Finding) {})
}

func (s *ProtectedStore) Reattach(id, taskID string) (Finding, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Finding{}, errors.New("task id is required")
	}
	return s.setState(id, FindingReattached, func(f *Finding) {
		f.LinkedTaskID = taskID
	})
}

func (s *ProtectedStore) Rescue(id string) (Finding, error) {
	unlock, err := s.lock()
	if err != nil {
		return Finding{}, err
	}
	defer unlock()

	rec, err := s.read()
	if err != nil {
		return Finding{}, err
	}
	for i := range rec.Findings {
		if rec.Findings[i].ID != id {
			continue
		}
		f := rec.Findings[i]
		info, err := rescueFinding(f, filepath.Join(filepath.Dir(s.path), "rescues"), s.now().UTC())
		if err != nil {
			return Finding{}, err
		}
		f.State = FindingRescued
		f.ResolvedAt = s.now().UTC()
		f.Rescue = info
		rec.Findings[i] = f
		if err := s.write(rec); err != nil {
			return Finding{}, err
		}
		return f, nil
	}
	return Finding{}, fmt.Errorf("cleanup finding %q not found", id)
}

func (s *ProtectedStore) setState(id string, state FindingState, mutate func(*Finding)) (Finding, error) {
	unlock, err := s.lock()
	if err != nil {
		return Finding{}, err
	}
	defer unlock()

	rec, err := s.read()
	if err != nil {
		return Finding{}, err
	}
	now := s.now().UTC()
	for i := range rec.Findings {
		if rec.Findings[i].ID != id {
			continue
		}
		f := rec.Findings[i]
		f.State = state
		f.ResolvedAt = now
		mutate(&f)
		rec.Findings[i] = f
		if err := s.write(rec); err != nil {
			return Finding{}, err
		}
		return f, nil
	}
	return Finding{}, fmt.Errorf("cleanup finding %q not found", id)
}

func protectedFindingID(kind ResourceKind, path, reason string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + filepath.Clean(path) + "\x00" + reason))
	return hex.EncodeToString(sum[:8])
}

func rescueFinding(f Finding, rescueDir string, now time.Time) (RescueInfo, error) {
	if err := os.MkdirAll(rescueDir, 0o755); err != nil {
		return RescueInfo{}, err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	switch f.Kind {
	case ResourceWorktree:
		ref := "refs/sybra-rescue/" + f.ID + "/" + stamp
		bundlePath := filepath.Join(rescueDir, f.ID+"-"+stamp+".bundle")
		if err := rescueWorktree(f.Path, ref, bundlePath); err != nil {
			return RescueInfo{}, err
		}
		return RescueInfo{Ref: ref, ArchivePath: bundlePath, VerifiedAt: now.UTC()}, nil
	case ResourceSandbox:
		archivePath := filepath.Join(rescueDir, f.ID+"-"+stamp+".tar.gz")
		if err := archiveDirectory(f.Path, archivePath); err != nil {
			return RescueInfo{}, err
		}
		if info, err := os.Stat(archivePath); err != nil || info.Size() == 0 {
			if err == nil {
				err = errors.New("sandbox rescue archive is empty")
			}
			return RescueInfo{}, err
		}
		return RescueInfo{ArchivePath: archivePath, VerifiedAt: now.UTC()}, nil
	default:
		return RescueInfo{}, fmt.Errorf("unsupported cleanup finding kind %q", f.Kind)
	}
}

func rescueWorktree(path, ref, bundlePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := gitexec.Run(ctx, gitexec.Options{Dir: path}, "update-ref", ref, "HEAD"); err != nil {
		return fmt.Errorf("create rescue ref: %w", err)
	}
	if _, err := gitexec.CombinedOutput(ctx, gitexec.Options{Dir: path}, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return fmt.Errorf("create rescue bundle: %w", err)
	}
	if out, err := gitexec.Output(ctx, gitexec.Options{Dir: path}, "rev-parse", "--verify", ref); err != nil || out == "" {
		if err == nil {
			err = errors.New("empty rescue ref")
		}
		return fmt.Errorf("verify rescue ref: %w", err)
	}
	info, err := os.Stat(bundlePath)
	if err != nil {
		return fmt.Errorf("stat rescue bundle: %w", err)
	}
	if info.Size() == 0 {
		return errors.New("rescue bundle is empty")
	}
	return nil
}

func archiveDirectory(root, dst string) error {
	file, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	gzw := gzip.NewWriter(file)
	tw := tar.NewWriter(gzw)

	rootFS, err := os.OpenRoot(root)
	if err != nil {
		_ = tw.Close()
		_ = gzw.Close()
		_ = file.Close()
		return err
	}
	defer rootFS.Close()

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = filepath.Base(root)
		} else {
			rel = filepath.Join(filepath.Base(root), rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		relToRoot, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := rootFS.Open(relToRoot)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})

	var closeErr error
	if err := tw.Close(); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if err := gzw.Close(); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if err := file.Close(); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if walkErr != nil {
		return errors.Join(walkErr, closeErr)
	}
	return closeErr
}

func ProtectedEvidenceLogPaths(logDir string, tasks []task.Task, findings []Finding) map[string]bool {
	out := make(map[string]bool)
	if logDir == "" {
		return out
	}
	taskMap := make(map[string]task.Task, len(tasks))
	for i := range tasks {
		taskMap[tasks[i].ID] = tasks[i]
	}
	for i := range findings {
		f := findings[i]
		if f.State != FindingOpen && f.State != FindingReattached {
			continue
		}
		taskID := strings.TrimSpace(f.EvidenceTaskID())
		if taskID == "" || taskID == unknownTaskID {
			continue
		}
		out[filepath.Join(logDir, "worktrees", taskID+"-setup.log")] = true
		tk, ok := taskMap[taskID]
		if !ok {
			continue
		}
		for j := range tk.AgentRuns {
			if strings.TrimSpace(tk.AgentRuns[j].LogFile) == "" {
				continue
			}
			out[filepath.Join(logDir, "agents", tk.AgentRuns[j].LogFile)] = true
		}
	}
	return out
}
