package evidence

import (
	"errors"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(artifact.New(t.TempDir()))
}

// fakeBlobStore is a narrow blobStore double letting tests simulate a
// read error or a corrupt/unparseable blob — states artifact.Store can reach
// (disk error, damaged file) but can't reliably trigger through its real API.
type fakeBlobStore struct {
	readErr  error
	readData []byte
}

func (f *fakeBlobStore) Put(taskID string, a artifact.Artifact) (artifact.Meta, error) {
	return artifact.Meta{}, nil
}

func (f *fakeBlobStore) Read(taskID, name string) ([]byte, artifact.Meta, error) {
	if f.readErr != nil {
		return nil, artifact.Meta{}, f.readErr
	}
	return f.readData, artifact.Meta{}, nil
}

func TestStore_Load_AbsentTaskReturnsZeroValue(t *testing.T) {
	s := newTestStore(t)
	ce, err := s.Load("no-such-task")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if ce.TaskID != "" || len(ce.Criteria) != 0 {
		t.Fatalf("Load on absent task = %+v, want zero value", ce)
	}
}

func TestStore_Load_ReadErrorFailsClosed(t *testing.T) {
	s := NewStore(&fakeBlobStore{readErr: errors.New("disk hiccup")})
	ce, err := s.Load("t1")
	if err == nil {
		t.Fatal("Load: want an error on an unreadable blob, got nil — a storage failure must not look like an absent baseline")
		panic("unreachable")
	}
	if len(ce.Criteria) != 0 {
		t.Errorf("Load on read error = %+v, want zero value", ce)
	}
}

func TestStore_Load_CorruptJSONFailsClosed(t *testing.T) {
	s := NewStore(&fakeBlobStore{readData: []byte("{not valid json")})
	ce, err := s.Load("t1")
	if err == nil {
		t.Fatal("Load: want an error on corrupt JSON, got nil — corrupted evidence must not look like an absent baseline")
		panic("unreachable")
	}
	if len(ce.Criteria) != 0 {
		t.Errorf("Load on corrupt JSON = %+v, want zero value", ce)
	}
}

func TestStore_Load_NilReceiverFailsOpen(t *testing.T) {
	var s *Store
	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load on nil store: %v", err)
		panic("unreachable")
	}
	if len(ce.Criteria) != 0 {
		t.Fatalf("Load on nil store = %+v, want zero value", ce)
	}
	if err := s.Append("t1", CriterionEvidence{Criterion: "x"}); err != nil {
		t.Fatalf("Append on nil store: %v", err)
	}
	if err := s.Rebind("t1", time.Now()); err != nil {
		t.Fatalf("Rebind on nil store: %v", err)
		panic("unreachable")
	}
}

func TestStore_Append_PersistsAndRoundTrips(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	entry := CriterionEvidence{
		Criterion:  "verify_checks",
		ProofType:  ProofDeterministicCheck,
		Command:    "go test ./...",
		ExitStatus: 0,
		FinalRev:   "abc123",
		Timestamp:  now,
	}
	if err := s.Append("t1", entry); err != nil {
		t.Fatalf("Append: %v", err)
		panic("unreachable")
	}

	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if ce.TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1", ce.TaskID)
	}
	if ce.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", ce.SchemaVersion, CurrentSchemaVersion)
	}
	got, ok := ce.ByCriterion("verify_checks")
	if !ok {
		t.Fatalf("verify_checks criterion missing after Append")
	}
	if got.FinalRev != "abc123" || got.Command != "go test ./..." {
		t.Errorf("round-tripped entry = %+v, want FinalRev=abc123 Command=go test ./...", got)
	}
}

func TestStore_Append_ReplacesSameCriterionInsteadOfGrowing(t *testing.T) {
	s := newTestStore(t)
	first := CriterionEvidence{Criterion: "verify_checks", ExitStatus: 1, FinalRev: "rev1"}
	second := CriterionEvidence{Criterion: "verify_checks", ExitStatus: 0, FinalRev: "rev2"}

	if err := s.Append("t1", first); err != nil {
		t.Fatalf("Append(first): %v", err)
		panic("unreachable")
	}
	if err := s.Append("t1", second); err != nil {
		t.Fatalf("Append(second): %v", err)
		panic("unreachable")
	}

	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if len(ce.Criteria) != 1 {
		t.Fatalf("Criteria = %d entries, want 1 (upsert, not append)", len(ce.Criteria))
	}
	got, ok := ce.ByCriterion("verify_checks")
	if !ok || got.FinalRev != "rev2" || got.ExitStatus != 0 {
		t.Errorf("latest entry = %+v, ok=%v — want the second (replacing) write to win", got, ok)
	}
}

func TestStore_Append_DistinctCriteriaCoexist(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("t1", CriterionEvidence{Criterion: "verify_checks", ExitStatus: 0}); err != nil {
		t.Fatalf("Append(verify_checks): %v", err)
	}
	if err := s.Append("t1", CriterionEvidence{Criterion: "detect_tampering", ExitStatus: 0}); err != nil {
		t.Fatalf("Append(detect_tampering): %v", err)
	}

	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if len(ce.Criteria) != 2 {
		t.Fatalf("Criteria = %d entries, want 2 distinct criteria", len(ce.Criteria))
	}
}

func TestStore_Rebind_ClearsPriorCriteria(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("t1", CriterionEvidence{Criterion: "verify_checks", ExitStatus: 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.Rebind("t1", at); err != nil {
		t.Fatalf("Rebind: %v", err)
		panic("unreachable")
	}

	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if len(ce.Criteria) != 0 {
		t.Fatalf("Criteria after Rebind = %d entries, want 0", len(ce.Criteria))
	}
	if !ce.UpdatedAt.Equal(at) {
		t.Errorf("UpdatedAt = %v, want %v", ce.UpdatedAt, at)
	}
	if ce.TaskID != "t1" {
		t.Errorf("TaskID after Rebind = %q, want t1", ce.TaskID)
	}
}

func TestStore_Append_AfterRebindStartsFreshGeneration(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("t1", CriterionEvidence{Criterion: "verify_checks", FinalRev: "stale-rev"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Rebind("t1", time.Now()); err != nil {
		t.Fatalf("Rebind: %v", err)
		panic("unreachable")
	}
	if err := s.Append("t1", CriterionEvidence{Criterion: "verify_checks", FinalRev: "fresh-rev"}); err != nil {
		t.Fatalf("Append after Rebind: %v", err)
	}

	ce, err := s.Load("t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if len(ce.Criteria) != 1 {
		t.Fatalf("Criteria = %d entries, want 1", len(ce.Criteria))
	}
	got, _ := ce.ByCriterion("verify_checks")
	if got.FinalRev != "fresh-rev" {
		t.Errorf("FinalRev = %q, want fresh-rev (stale pre-rebind entry must not survive)", got.FinalRev)
	}
}
