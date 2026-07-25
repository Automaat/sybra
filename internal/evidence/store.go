package evidence

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
)

// blobName is the single per-task blob CompletionEvidence round-trips
// through — one artifact, not one per criterion, so Load/Append/Rebind always
// see (and persist) the whole set atomically.
const blobName = "completion-evidence.json"

// blobStore is the narrow slice of artifact.Store's API the evidence store
// needs, so tests can fake it without standing up a real artifact.Store.
type blobStore interface {
	Put(taskID string, a artifact.Artifact) (artifact.Meta, error)
	Read(taskID, name string) ([]byte, artifact.Meta, error)
}

// Store persists CompletionEvidence over a blobStore (in production,
// internal/artifact.Store). Local-debug/audit data — see the package doc.
type Store struct {
	blobs blobStore
}

// NewStore wraps blobs as an evidence Store.
func NewStore(blobs blobStore) *Store {
	return &Store{blobs: blobs}
}

// Load reads a task's CompletionEvidence. Fails open: a never-recorded task
// (blob absent) or a corrupt/unreadable blob both return a zero-value
// CompletionEvidence and a nil error — callers (the require_evidence gate)
// treat an empty result as "no baseline yet", not as a hard failure, so a
// storage hiccup can never itself strand a task.
func (s *Store) Load(taskID string) (CompletionEvidence, error) {
	if s == nil || s.blobs == nil {
		return CompletionEvidence{}, nil
	}
	data, _, err := s.blobs.Read(taskID, blobName)
	if err != nil {
		if !errors.Is(err, artifact.ErrNotFound) {
			slog.Warn("evidence.load.read-err", "task_id", taskID, "err", err)
		}
		return CompletionEvidence{}, nil
	}
	var ce CompletionEvidence
	if jErr := json.Unmarshal(data, &ce); jErr != nil {
		slog.Warn("evidence.load.parse-err", "task_id", taskID, "err", jErr)
		return CompletionEvidence{}, nil
	}
	return ce, nil
}

// Append records one CriterionEvidence entry, replacing any existing entry
// for the same Criterion — so the set always reflects the latest proof per
// criterion rather than growing unbounded across retries/re-asks. This is a
// read-modify-write over the single blob; callers that invoke it from a
// best-effort recording path (every deterministic gate step) must not let its
// error alter their own pass/fail outcome or timing.
func (s *Store) Append(taskID string, entry CriterionEvidence) error {
	if s == nil || s.blobs == nil {
		return nil
	}
	ce, err := s.Load(taskID)
	if err != nil {
		return err
	}
	ce.TaskID = taskID
	if ce.SchemaVersion == 0 {
		ce.SchemaVersion = CurrentSchemaVersion
	}
	ce.Criteria = upsertCriterion(ce.Criteria, entry)
	ce.UpdatedAt = entry.Timestamp
	return s.save(taskID, ce)
}

// Rebind resets a task's CompletionEvidence to an empty set, discarding every
// prior criterion entry. Used to explicitly invalidate accumulated evidence
// when the underlying generation it was collected against (contract, base
// branch, environment) has changed — callers that rebind must expect every
// required criterion to look "missing" again until its producer reruns.
func (s *Store) Rebind(taskID string, at time.Time) error {
	if s == nil || s.blobs == nil {
		return nil
	}
	ce := CompletionEvidence{
		SchemaVersion: CurrentSchemaVersion,
		TaskID:        taskID,
		UpdatedAt:     at,
	}
	return s.save(taskID, ce)
}

func (s *Store) save(taskID string, ce CompletionEvidence) error {
	data, err := json.MarshalIndent(ce, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.blobs.Put(taskID, artifact.Artifact{
		Kind:    artifact.KindGeneric,
		Name:    blobName,
		Content: data,
	})
	return err
}

// upsertCriterion replaces the entry sharing entry.Criterion, or appends it
// when no such entry exists yet.
func upsertCriterion(list []CriterionEvidence, entry CriterionEvidence) []CriterionEvidence {
	for i := range list {
		if list[i].Criterion == entry.Criterion {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}
