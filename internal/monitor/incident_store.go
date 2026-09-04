package monitor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"
)

const incidentVersion = 1
const incidentStoreLockTimeout = 5 * time.Second
const maxAffectedTasks = 256
const maxRemediationAttempts = 100

type IncidentStore struct {
	dir   string
	store IncidentPersistence
	mu    sync.Mutex
}

// NewIncidentStoreWith returns a store persisting through p rather than to files.
func NewIncidentStoreWith(dir string, p IncidentPersistence) (*IncidentStore, error) {
	if p == nil {
		return nil, errors.New("incident store: nil persistence")
	}
	return &IncidentStore{dir: dir, store: p}, nil
}

func NewIncidentStore(dir string) (*IncidentStore, error) {
	if dir == "" {
		return nil, errors.New("incident store: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("incident store: mkdir: %w", err)
	}
	return &IncidentStore{dir: dir, store: newIncidentFiles(dir)}, nil
}

func (s *IncidentStore) Observe(a Anomaly, cause RootCause, safeTaskID string) (Incident, IncidentChange, error) {
	unlock, err := s.store.Lock()
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
			in.SupersededAt = nil
			in.SupersededByConfig = ""
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
	overflowBefore := in.AffectedTaskOverflow
	if addAffectedTask(&in, safeTaskID) && change == IncidentUnchanged && !suppressedObservation {
		change = IncidentExpanded
	}
	if in.AffectedTaskOverflow != overflowBefore && change == IncidentUnchanged && !suppressedObservation {
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
	unlock, err := s.store.Lock()
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

func (s *IncidentStore) ReconcileHealthy(seen, observableScopes, coveredFailureCodes map[string]bool, configGenerations map[string]string, now time.Time, grace, reopenGrace time.Duration) ([]Incident, error) {
	unlock, err := s.store.Lock()
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
		if current := configGenerations[in.FailureCode]; current != "" && current != in.ConfigGeneration {
			in.State = IncidentResolved
			in.Revision++
			superseded := now
			in.SupersededAt = &superseded
			in.SupersededByConfig = current
			if err := s.save(in); err != nil {
				return nil, err
			}
			closed = append(closed, in)
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
		var containedAt *time.Time
		for j := range in.RemediationAttempts {
			if in.RemediationAttempts[j].ObservedAt == nil && in.RemediationAttempts[j].Result == "attempted" {
				in.RemediationAttempts[j].Result = "observed_success"
				in.RemediationAttempts[j].ObservedAt = &resolved
				if containedAt == nil || in.RemediationAttempts[j].AttemptedAt.After(*containedAt) {
					contained := in.RemediationAttempts[j].AttemptedAt
					containedAt = &contained
				}
			}
		}
		if in.FirstContainedAt == nil && containedAt != nil {
			in.FirstContainedAt = containedAt
		}
		if err := s.save(in); err != nil {
			return nil, err
		}
		closed = append(closed, in)
	}
	return closed, nil
}

func (s *IncidentStore) Link(fp, issueURL, prURL string, duplicates []int) error {
	unlock, err := s.store.Lock()
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
	unlock, err := s.store.Lock()
	if err != nil {
		return Incident{}, false, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(fp)
}

func (s *IncidentStore) List() ([]Incident, error) {
	unlock, err := s.store.Lock()
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

func (s *IncidentStore) load(fp string) (Incident, bool, error) { return s.store.Load(fp) }

func (s *IncidentStore) save(in Incident) error { return s.store.Save(in) }

func (s *IncidentStore) list() ([]Incident, error) { return s.store.List() }

func slicesSortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
