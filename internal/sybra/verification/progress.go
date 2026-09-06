package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/reviewprogress"
)

// ProgressScope is host-authored and stable only across retries of one review
// step in one workflow execution. No agent may choose its own lineage/input.
type ProgressScope struct {
	BaseRef        string `json:"baseRef"`
	Lineage        string `json:"lineage"`
	ContractDigest string `json:"contractDigest"`
}

type ProgressInput struct {
	Scope   ProgressScope `json:"scope"`
	TaskID  string        `json:"taskId"`
	Role    string        `json:"role"`
	HeadSHA string        `json:"headSha"`
	BaseSHA string        `json:"baseSha"`
}

type progressRecord struct {
	Input          ProgressInput           `json:"input"`
	LeaseID        string                  `json:"leaseId"`
	AgentID        string                  `json:"agentId"`
	AttemptStarted time.Time               `json:"attemptStarted"`
	Closed         bool                    `json:"closed"`
	Progress       reviewprogress.Progress `json:"progress"`
}

func validDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

// PrepareProgress seeds only a verifier-owned checkpoint from the same exact
// input. Storage is beside lease metadata, outside every agent writable root.
func (m *Manager) PrepareProgress(ctx context.Context, lease Lease, scope ProgressScope) (Lease, string, error) {
	if lease.Purpose != "review" || lease.WorkspaceDir == "" || scope.BaseRef == "" || !validDigest(scope.Lineage) || !validDigest(scope.ContractDigest) {
		return lease, "", nil
	}
	base, err := gitOutput(ctx, lease.CanonicalDir, "rev-parse", "--verify", "--end-of-options", scope.BaseRef+"^{commit}")
	if err != nil {
		return lease, "", errors.New("review progress: exact review base unavailable")
	}
	// A local clone's origin/main is the source's local main, not its fetched
	// origin/main. Import the authoritative object and pin the comparison ref.
	if _, err := command(ctx, lease.WorkspaceDir, "fetch", "--no-tags", lease.CanonicalDir, base); err != nil {
		return lease, "", errors.New("review progress: cannot import exact review base")
	}
	if err := reviewprogress.PinBase(ctx, lease.WorkspaceDir, scope.BaseRef, base); err != nil {
		return lease, "", err
	}
	// Ref history remains an attempt-local tamper fence in ValidateSource.
	// A watchdog's no-op reset must not invalidate cross-attempt continuity.
	input := ProgressInput{Scope: scope, TaskID: lease.TaskID, Role: lease.Purpose, HeadSHA: lease.SourceSHA, BaseSHA: base}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease.ProgressInput = &input
	if err := m.saveLease(lease); err != nil {
		return lease, "", err
	}
	record, err := m.loadProgress(input)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("review.progress.invalid_cache", "lease_id", lease.ID)
	}
	if err == nil && record.Input == input && !record.Closed && record.Progress.Validate() == nil {
		return lease, reviewprogress.Prompt(&record.Progress), nil
	}
	return lease, reviewprogress.Prompt(nil), nil
}

// CaptureProgress is called only from the exact bound review agent's terminal
// callback, before lease cleanup. Partial data never enters a final sidecar.
// Passing closed retires a successful attempt's notes so a fresh reviewer does
// not inherit a completed review's provisional opinions.
func (m *Manager) CaptureProgress(ctx context.Context, lease Lease, agentID, role string, packets []string, closed bool) error {
	if lease.ProgressInput == nil || lease.AgentID != agentID || role != "review" || lease.Purpose != role {
		return nil
	}
	input := *lease.ProgressInput
	if input.TaskID != lease.TaskID || input.Role != role || input.HeadSHA != lease.SourceSHA {
		return errors.New("review progress: lease input mismatch")
	}
	if err := m.ValidateSource(ctx, lease); err != nil {
		return err
	}
	if err := reviewprogress.ValidateWorkspace(ctx, lease.WorkspaceDir, input.HeadSHA, input.Scope.BaseRef, input.BaseSHA); err != nil {
		return err
	}
	var progress reviewprogress.Progress
	found := false
	for _, packet := range slices.Backward(packets) {
		parsed, err := reviewprogress.Parse(packet)
		if err == nil {
			progress, found = parsed, true
			break
		}
	}
	if !found && !closed {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.loadProgress(input)
	if err == nil && (stored.AttemptStarted.After(lease.CreatedAt) || (stored.AttemptStarted.Equal(lease.CreatedAt) && stored.LeaseID != lease.ID)) {
		return nil
	}
	if closed {
		progress = reviewprogress.Progress{}
	}
	record := progressRecord{Input: input, LeaseID: lease.ID, AgentID: agentID, AttemptStarted: lease.CreatedAt, Closed: closed, Progress: progress}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := m.progressPath(input)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.AtomicWriteMode(path, data, 0o600)
}

func (m *Manager) progressPath(input ProgressInput) string {
	return filepath.Join(m.root, "review-progress", input.Key()+".json")
}

// Key binds execution-host proof to the entire host-authored input/lineage,
// including contract changes that do not advance the task generation.
func (input ProgressInput) Key() string {
	data, _ := json.Marshal(input)
	key := sha256.Sum256(data)
	return hex.EncodeToString(key[:])
}

func (m *Manager) loadProgress(input ProgressInput) (progressRecord, error) {
	var record progressRecord
	info, err := os.Lstat(m.progressPath(input))
	if err != nil {
		return record, err
	}
	const maxRecordBytes = reviewprogress.MaxBytes + 4096
	if !info.Mode().IsRegular() || info.Size() > maxRecordBytes {
		return record, errors.New("review progress: invalid persisted record")
	}
	file, err := os.Open(m.progressPath(input))
	if err != nil {
		return record, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil || len(data) > maxRecordBytes || json.Unmarshal(data, &record) != nil {
		return record, errors.New("review progress: malformed persisted record")
	}
	if record.Input != input || record.LeaseID == "" || record.AgentID == "" || record.AttemptStarted.IsZero() {
		return record, errors.New("review progress: invalid persisted identity")
	}
	return record, nil
}
