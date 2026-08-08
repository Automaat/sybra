package monitor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"gopkg.in/yaml.v3"
)

const incidentVersion = 1
const incidentStoreLockTimeout = 5 * time.Second
const maxAffectedTasks = 256
const maxRemediationAttempts = 100

type IncidentStore struct {
	dir string
	mu  sync.Mutex
}

func NewIncidentStore(dir string) (*IncidentStore, error) {
	if dir == "" {
		return nil, errors.New("incident store: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("incident store: mkdir: %w", err)
	}
	return &IncidentStore{dir: dir}, nil
}

func (s *IncidentStore) Observe(a Anomaly, cause RootCause, safeTaskID string) (Incident, IncidentChange, error) {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return Incident{}, IncidentUnchanged, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := RootCauseFingerprint(cause)
	in, exists, err := s.load(fp)
	if err != nil {
		return Incident{}, IncidentUnchanged, err
	}
	change := IncidentUnchanged
	suppressedObservation := false
	if !exists {
		in = Incident{Version: incidentVersion, Revision: 1, Fingerprint: fp, FailureCode: cause.FailureCode,
			Component: cause.Component, Capability: cause.Capability, ProjectScope: cause.ProjectScope,
			ConfigGeneration: cause.ConfigGeneration, Confidential: a.Confidential, State: IncidentActive, FirstSeen: a.DetectedAt,
			LastSeen: a.DetectedAt, LatestEvidence: certifiedEvidence(a)}
		change = IncidentOpened
	} else {
		switch {
		case in.State == IncidentResolved:
			in.State = IncidentActive
			in.ResolvedAt = nil
			in.HealthySince = nil
			in.RecurrenceCount++
			if in.ReopenGraceUntil != nil && a.DetectedAt.Before(*in.ReopenGraceUntil) {
				until := *in.ReopenGraceUntil
				in.SuppressedUntil = &until
				suppressedObservation = true
			} else {
				change = IncidentReopened
				in.SuppressedUntil = nil
			}
		case in.SuppressedUntil != nil && !a.DetectedAt.Before(*in.SuppressedUntil):
			change = IncidentReopened
			in.SuppressedUntil = nil
		case in.SuppressedUntil != nil:
			suppressedObservation = true
		}
		for i := range in.RemediationAttempts {
			attempt := &in.RemediationAttempts[i]
			if attempt.Result == "attempted" && attempt.AttemptedAt.Before(a.DetectedAt) {
				observed := a.DetectedAt
				attempt.Result, attempt.ObservedAt = "observed_failure", &observed
				if !suppressedObservation && change == IncidentUnchanged {
					change = IncidentExpanded
				}
			}
		}
		in.LastSeen = a.DetectedAt
		if ev := certifiedEvidence(a); ev.Proven {
			semanticChange := ev.Fingerprint != "" && ev.Fingerprint != in.LatestEvidence.Fingerprint
			in.LatestEvidence = ev
			if semanticChange && change == IncidentUnchanged && !suppressedObservation {
				change = IncidentExpanded
			}
		}
	}
	if addAffectedTask(&in, safeTaskID) && change == IncidentUnchanged && !suppressedObservation {
		change = IncidentExpanded
	}
	if len(in.AffectedTaskIDs) > maxAffectedTasks {
		in.AffectedTaskIDs = slices.Delete(in.AffectedTaskIDs, 0, len(in.AffectedTaskIDs)-maxAffectedTasks)
	}
	in.AffectedTaskCount = max(in.AffectedTaskCount, len(in.AffectedTaskIDs))
	in.HealthySince = nil
	if exists && change != IncidentUnchanged {
		in.Revision++
	}
	if err := s.save(in); err != nil {
		return Incident{}, IncidentUnchanged, err
	}
	return in, change, nil
}

func (s *IncidentStore) RecordRemediation(fp, kind, result string, at time.Time) error {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok, err := s.load(fp)
	if err != nil || !ok {
		return err
	}
	in.RemediationAttempts = append(in.RemediationAttempts, RemediationAttempt{ID: remediationAttemptID(fp, kind, at, len(in.RemediationAttempts)), AttemptedAt: at, Kind: kind, Result: result})
	in.Revision++
	if len(in.RemediationAttempts) > maxRemediationAttempts {
		in.RemediationAttempts = slices.Delete(in.RemediationAttempts, 0, len(in.RemediationAttempts)-maxRemediationAttempts)
	}
	return s.save(in)
}

func (s *IncidentStore) ReconcileHealthy(seen, observableScopes, coveredFailureCodes map[string]bool, _ map[string]string, now time.Time, grace, reopenGrace time.Duration) ([]Incident, error) {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.list()
	if err != nil {
		return nil, err
	}
	var closed []Incident
	for i := range all {
		in := all[i]
		if in.State != IncidentActive || seen[in.Fingerprint] || !observableScopes[in.ProjectScope] ||
			!coveredFailureCodes[in.FailureCode] {
			continue
		}
		if in.HealthySince == nil {
			at := now
			in.HealthySince = &at
			if err := s.save(in); err != nil {
				return nil, err
			}
			continue
		}
		if now.Sub(*in.HealthySince) < grace {
			continue
		}
		in.State = IncidentResolved
		in.Revision++
		resolved := now
		in.ResolvedAt = &resolved
		reopenAt := now.Add(reopenGrace)
		in.ReopenGraceUntil = &reopenAt
		for j := range slices.Backward(in.RemediationAttempts) {
			if in.RemediationAttempts[j].ObservedAt == nil && in.RemediationAttempts[j].Result == "attempted" {
				in.RemediationAttempts[j].Result = "observed_success"
				in.RemediationAttempts[j].ObservedAt = &resolved
				if in.FirstContainedAt == nil {
					contained := in.RemediationAttempts[j].AttemptedAt
					in.FirstContainedAt = &contained
				}
				break
			}
		}
		if err := s.save(in); err != nil {
			return nil, err
		}
		closed = append(closed, in)
	}
	return closed, nil
}

func (s *IncidentStore) Link(fp, issueURL, prURL string, duplicates []int) error {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok, err := s.load(fp)
	if err != nil {
		return err
	}
	if !ok {
		return fs.ErrNotExist
	}
	if issueURL != "" {
		in.IssueURL = issueURL
	}
	if prURL != "" {
		in.PRURL = prURL
	}
	in.PublishedRevision = in.Revision
	if duplicates != nil {
		in.DuplicateIssues = append([]int(nil), duplicates...)
		slicesSortInts(in.DuplicateIssues)
	}
	return s.save(in)
}

func (s *IncidentStore) Get(fp string) (Incident, bool, error) {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return Incident{}, false, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(fp)
}

func (s *IncidentStore) List() ([]Incident, error) {
	unlock, err := fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

func (s *IncidentStore) path(fp string) string { return filepath.Join(s.dir, fp+".yaml") }

func (s *IncidentStore) load(fp string) (Incident, bool, error) {
	data, err := os.ReadFile(s.path(fp))
	if errors.Is(err, fs.ErrNotExist) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("incident store: read: %w", err)
	}
	var in Incident
	if err := yaml.Unmarshal(data, &in); err != nil {
		return Incident{}, false, fmt.Errorf("incident store: decode: %w", err)
	}
	return in, true, nil
}

func (s *IncidentStore) save(in Incident) error {
	data, err := yaml.Marshal(in)
	if err != nil {
		return fmt.Errorf("incident store: encode: %w", err)
	}
	if err := fsutil.AtomicWriteMode(s.path(in.Fingerprint), data, 0o600); err != nil {
		return fmt.Errorf("incident store: write: %w", err)
	}
	return nil
}

func (s *IncidentStore) list() ([]Incident, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("incident store: list: %w", err)
	}
	out := make([]Incident, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var in Incident
		if err := yaml.Unmarshal(data, &in); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

func slicesSortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
