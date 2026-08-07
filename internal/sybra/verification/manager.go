// Package verification owns disposable workspaces used by independent
// verification. A verifier gets a writable clone at one exact source commit;
// its Git metadata and mutations are discarded after evidence is captured.
package verification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/textutil"
	"github.com/google/uuid"
)

var (
	ErrSourceMoved = errors.New("verification source moved; evidence is stale")
	ErrSourceDirty = errors.New("verification source became dirty; evidence is stale")
)

type Lease struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"taskId"`
	Purpose        string    `json:"purpose"`
	CanonicalDir   string    `json:"canonicalDir"`
	WorkspaceDir   string    `json:"workspaceDir"`
	ScratchDir     string    `json:"scratchDir"`
	SourceSHA      string    `json:"sourceSha"`
	SourceRefState string    `json:"sourceRefState"`
	AgentID        string    `json:"agentId,omitempty"`
	CertificateID  string    `json:"certificateId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Report struct {
	LeaseID       string    `json:"leaseId"`
	TaskID        string    `json:"taskId"`
	Purpose       string    `json:"purpose"`
	SourceSHA     string    `json:"sourceSha"`
	ObservedSHA   string    `json:"observedSourceSha,omitempty"`
	WorkspaceSHA  string    `json:"workspaceSha,omitempty"`
	CertificateID string    `json:"certificateId,omitempty"`
	Commands      []string  `json:"commands,omitempty"`
	Output        string    `json:"output,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	Status        string    `json:"status"`
	FinishedAt    time.Time `json:"finishedAt"`
}

type Manager struct {
	root         string
	artifacts    *artifact.Store
	logger       *slog.Logger
	grantRevoker func(string) error
	mu           sync.Mutex
}

func New(root string, artifacts *artifact.Store, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{root: root, artifacts: artifacts, logger: logger}
}

// SetGrantRevoker installs the durable-control cleanup used before an
// abandoned verifier lease is removed during startup reconciliation.
func (m *Manager) SetGrantRevoker(revoke func(string) error) {
	m.mu.Lock()
	m.grantRevoker = revoke
	m.mu.Unlock()
}

func (m *Manager) Prepare(ctx context.Context, taskID, purpose, canonicalDir string) (Lease, error) {
	canonicalDir, err := filepath.Abs(canonicalDir)
	if err != nil {
		return Lease{}, fmt.Errorf("resolve canonical worktree: %w", err)
	}
	sha, err := gitOutput(ctx, canonicalDir, "rev-parse", "HEAD")
	if err != nil {
		return Lease{}, fmt.Errorf("resolve authoritative source: %w", err)
	}
	status, err := gitOutput(ctx, canonicalDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return Lease{}, fmt.Errorf("inspect authoritative source: %w", err)
	}
	if status != "" {
		return Lease{}, errors.New("authoritative worktree is not reconciled: commit or discard changes before verification")
	}
	refState, err := gitRefState(ctx, canonicalDir)
	if err != nil {
		return Lease{}, fmt.Errorf("snapshot authoritative ref history: %w", err)
	}
	id := uuid.NewString()
	runDir := filepath.Join(m.root, "runs", id)
	workspaceDir := filepath.Join(runDir, "source")
	scratchDir := filepath.Join(runDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return Lease{}, fmt.Errorf("create verification workspace: %w", err)
	}
	if out, err := command(ctx, "", "clone", "--no-hardlinks", "--no-checkout", canonicalDir, workspaceDir); err != nil {
		_ = os.RemoveAll(runDir)
		return Lease{}, fmt.Errorf("clone authoritative source: %w: %s", err, out)
	}
	if out, err := command(ctx, workspaceDir, "checkout", "--detach", sha); err != nil {
		_ = os.RemoveAll(runDir)
		return Lease{}, fmt.Errorf("checkout authoritative source %s: %w: %s", sha, err, out)
	}
	// A verifier must not be able to promote its local refs or commits.
	if out, err := command(ctx, workspaceDir, "remote", "set-url", "--push", "origin", "disabled://verification-workspace"); err != nil {
		_ = os.RemoveAll(runDir)
		return Lease{}, fmt.Errorf("disable verification push: %w: %s", err, out)
	}
	lease := Lease{ID: id, TaskID: taskID, Purpose: purpose, CanonicalDir: canonicalDir, WorkspaceDir: workspaceDir, ScratchDir: scratchDir, SourceSHA: sha, SourceRefState: refState, CreatedAt: time.Now().UTC()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveLease(lease); err != nil {
		_ = os.RemoveAll(runDir)
		return Lease{}, err
	}
	return lease, nil
}

// PrepareScratch creates a durable, lease-owned writable home for a verifier
// that intentionally has no canonical worktree (for example a best-of-N
// judge). It still participates in bind/reconcile/release so concurrent judges
// never share credentials or mutable provider state across restarts.
func (m *Manager) PrepareScratch(taskID, purpose string) (Lease, error) {
	id := uuid.NewString()
	runDir := filepath.Join(m.root, "runs", id)
	scratchDir := filepath.Join(runDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return Lease{}, fmt.Errorf("create verification scratch: %w", err)
	}
	lease := Lease{ID: id, TaskID: taskID, Purpose: purpose, ScratchDir: scratchDir, CreatedAt: time.Now().UTC()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveLease(lease); err != nil {
		_ = os.RemoveAll(runDir)
		return Lease{}, err
	}
	return lease, nil
}

func (m *Manager) BindAgent(leaseID, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, err := m.loadLease(leaseID)
	if err != nil {
		return err
	}
	lease.AgentID = agentID
	return m.saveLease(lease)
}

func (m *Manager) Lease(id string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLease(id)
}

// SetCertificateForWorkspace attaches the run-environment admission proof to
// the durable lease before the process starts.
func (m *Manager) SetCertificateForWorkspace(workspaceDir, certificateID string) error {
	if strings.TrimSpace(workspaceDir) == "" || strings.TrimSpace(certificateID) == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := os.ReadDir(filepath.Join(m.root, "leases"))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		lease, err := m.loadLease(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil && lease.WorkspaceDir == workspaceDir {
			lease.CertificateID = certificateID
			return m.saveLease(lease)
		}
	}
	return nil
}

func (m *Manager) LeaseForAgent(agentID string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := os.ReadDir(filepath.Join(m.root, "leases"))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		lease, err := m.loadLease(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil && lease.AgentID == agentID {
			return lease, true
		}
	}
	return Lease{}, false
}

// Finalize captures the verifier's complete mutation footprint and rejects it
// when the authoritative branch moved since materialization. It intentionally
// does not delete the workspace; Release runs after consumers import any
// structured files produced by a test runner.
func (m *Manager) Finalize(ctx context.Context, lease Lease, commands []string, output, certificateID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	observed, observedErr := gitOutput(ctx, lease.CanonicalDir, "rev-parse", "HEAD")
	observedRefState, refStateErr := gitRefState(ctx, lease.CanonicalDir)
	canonicalStatus, canonicalStatusErr := gitOutput(ctx, lease.CanonicalDir, "status", "--porcelain", "--untracked-files=all")
	workspaceSHA, workspaceErr := gitOutput(ctx, lease.WorkspaceDir, "rev-parse", "HEAD")
	// Compare against the authoritative source commit, not the verifier's HEAD:
	// tests are allowed to create commits in their private Git metadata and
	// those mutations are still evidence that must be captured and discarded.
	diff, diffErr := captureWorkspaceDiff(ctx, lease.WorkspaceDir, lease.SourceSHA)
	status, statusErr := gitOutput(ctx, lease.WorkspaceDir, "status", "--porcelain", "--untracked-files=all")
	if status != "" {
		diff += "\n[verification status]\n" + status + "\n"
	}
	if strings.TrimSpace(certificateID) == "" {
		certificateID = lease.CertificateID
	}
	report := Report{LeaseID: lease.ID, TaskID: lease.TaskID, Purpose: lease.Purpose, SourceSHA: lease.SourceSHA, ObservedSHA: observed, WorkspaceSHA: workspaceSHA, CertificateID: certificateID, Commands: commands, Output: bounded(output, 64<<10), Diff: bounded(diff, 256<<10), Status: "accepted", FinishedAt: time.Now().UTC()}
	if observedErr != nil || refStateErr != nil || canonicalStatusErr != nil || observed != lease.SourceSHA || observedRefState != lease.SourceRefState || canonicalStatus != "" {
		report.Status = "stale"
	} else if workspaceErr != nil || diffErr != nil || statusErr != nil {
		report.Status = "invalid"
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil && m.artifacts != nil {
		_, err = m.artifacts.Put(lease.TaskID, artifact.Artifact{Kind: artifact.KindGeneric, Name: "verification-" + lease.ID + ".json", ProducerRole: lease.Purpose, Content: data})
	}
	if err != nil {
		return fmt.Errorf("record verification evidence: %w", err)
	}
	if report.Status == "stale" {
		if canonicalStatusErr != nil {
			return fmt.Errorf("inspect authoritative source status: %w", canonicalStatusErr)
		}
		if canonicalStatus != "" {
			return fmt.Errorf("%w: %s", ErrSourceDirty, bounded(canonicalStatus, 4<<10))
		}
		return fmt.Errorf("%w: expected %s, observed %s", ErrSourceMoved, lease.SourceSHA, observed)
	}
	if workspaceErr != nil {
		return fmt.Errorf("inspect verification workspace HEAD: %w", workspaceErr)
	}
	if diffErr != nil {
		return fmt.Errorf("capture verification workspace diff: %w", diffErr)
	}
	if statusErr != nil {
		return fmt.Errorf("inspect verification workspace status: %w", statusErr)
	}
	return nil
}

// ValidateSource rechecks the authoritative worktree without rewriting the
// captured verifier report. Coordinators call it after parallel peers join so
// movement during a slower sibling gate cannot race past finalization.
func (m *Manager) ValidateSource(ctx context.Context, lease Lease) error {
	observed, err := gitOutput(ctx, lease.CanonicalDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve authoritative source: %w", err)
	}
	refState, err := gitRefState(ctx, lease.CanonicalDir)
	if err != nil {
		return fmt.Errorf("snapshot authoritative ref history: %w", err)
	}
	status, err := gitOutput(ctx, lease.CanonicalDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect authoritative source status: %w", err)
	}
	if status != "" {
		return fmt.Errorf("%w: %s", ErrSourceDirty, bounded(status, 4<<10))
	}
	if observed != lease.SourceSHA || refState != lease.SourceRefState {
		return fmt.Errorf("%w: expected %s, observed %s", ErrSourceMoved, lease.SourceSHA, observed)
	}
	return nil
}

func gitRefState(ctx context.Context, dir string) (string, error) {
	headState, err := command(ctx, dir, "reflog", "show", "--format=%H%x00%gD%x00%gs", "HEAD")
	if err != nil {
		return "", err
	}
	branch, branchErr := command(ctx, dir, "symbolic-ref", "-q", "HEAD")
	branch = strings.TrimSpace(branch)
	if branchErr != nil && branch != "" {
		return "", branchErr
	}
	branchState := ""
	if branch != "" {
		branchState, err = command(ctx, dir, "reflog", "show", "--format=%H%x00%gD%x00%gs", branch)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(headState+"\x00"+branch+"\x00"+branchState))), nil
}

func captureWorkspaceDiff(ctx context.Context, workspaceDir, sourceSHA string) (string, error) {
	diff, err := command(ctx, workspaceDir, "diff", "--binary", "--no-ext-diff", sourceSHA)
	if err != nil {
		return "", err
	}
	var extra strings.Builder
	raw, err := command(ctx, workspaceDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	for rel := range strings.SplitSeq(raw, "\x00") {
		if rel == "" {
			continue
		}
		path := filepath.Join(workspaceDir, filepath.Clean(rel))
		if !strings.HasPrefix(path, workspaceDir+string(os.PathSeparator)) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read untracked verification output %q: %w", rel, err)
		}
		_, _ = fmt.Fprintf(&extra, "\ndiff --sybra-untracked a/%s b/%s\nnew file mode %06o\n--- /dev/null\n+++ b/%s\n%s", rel, rel, info.Mode().Perm(), rel, bounded(string(content), 64<<10))
		if len(content) == 0 || content[len(content)-1] != '\n' {
			extra.WriteByte('\n')
		}
	}
	return diff + extra.String(), nil
}

func (m *Manager) Release(lease Lease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLeaseRun(lease)
	_ = os.Remove(m.leasePath(lease.ID))
}

func (m *Manager) removeLeaseRun(lease Lease) {
	expected := filepath.Clean(filepath.Join(m.root, "runs", lease.ID))
	actual := ""
	if lease.ScratchDir != "" {
		actual = filepath.Clean(filepath.Dir(lease.ScratchDir))
	} else if lease.WorkspaceDir != "" {
		actual = filepath.Clean(filepath.Dir(lease.WorkspaceDir))
	}
	if actual == expected {
		_ = os.RemoveAll(expected)
	}
}

// Reconcile removes abandoned leases while retaining workspaces belonging to
// reattached live agents. A missing lease is harmless and cleanup is idempotent.
func (m *Manager) Reconcile(activeAgentIDs map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := os.ReadDir(filepath.Join(m.root, "leases"))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		lease, err := m.loadLease(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			_ = os.Remove(filepath.Join(m.root, "leases", entry.Name()))
			continue
		}
		if _, ok := activeAgentIDs[lease.AgentID]; ok && lease.AgentID != "" {
			continue
		}
		if m.grantRevoker != nil {
			if strings.TrimSpace(lease.ScratchDir) == "" {
				m.logger.Error("verification.reconcile.revoke", "lease_id", lease.ID, "err", "lease has no trusted scratch directory")
				continue
			}
			if err := m.grantRevoker(lease.ScratchDir); err != nil {
				// Preserve the durable lease and scratch path so the next startup
				// can retry revocation. Deleting them here would leave an
				// untraceable bearer grant valid until expiry.
				m.logger.Error("verification.reconcile.revoke", "lease_id", lease.ID, "scratch_dir", lease.ScratchDir, "err", err)
				continue
			}
		}
		m.removeLeaseRun(lease)
		_ = os.Remove(m.leasePath(lease.ID))
	}
}

func (m *Manager) leasePath(id string) string { return filepath.Join(m.root, "leases", id+".json") }

func (m *Manager) saveLease(lease Lease) error {
	if err := os.MkdirAll(filepath.Join(m.root, "leases"), 0o700); err != nil {
		return fmt.Errorf("create verification lease directory: %w", err)
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWriteMode(m.leasePath(lease.ID), data, 0o600); err != nil {
		return fmt.Errorf("write verification lease: %w", err)
	}
	return nil
}

func (m *Manager) loadLease(id string) (Lease, error) {
	data, err := os.ReadFile(m.leasePath(id))
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := command(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

func command(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{Dir: dir}, args...)
	return string(out), err
}

func bounded(s string, limit int) string {
	return textutil.TruncateMiddle(s, limit, "\n... truncated ...\n")
}
