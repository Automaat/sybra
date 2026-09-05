package monitor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/fsutil"
)

// IncidentPersistence is where the incident ledger lives. Two implementations
// exist: the per-fingerprint YAML files and the database-backed
// SQLIncidentStore, selected by config.Database.Backend.
//
// Lock acquires the exclusive hold a read-modify-write needs and returns its
// release. Every public method on IncidentStore is one such cycle — observe,
// remediate, reconcile, link — and losing one loses an operator's record that
// something failed, or re-files an issue that was already filed.
//
// A handle rather than a callback because that is the shape the store already
// had: the database implementation begins a transaction on Lock and commits it
// on release, which is the same contract an advisory file lock offers.
type IncidentPersistence interface {
	Lock() (release func() error, err error)
	Load(fingerprint string) (Incident, bool, error)
	Save(in Incident) error
	List() ([]Incident, error)
}

// incidentFiles keeps incidents as one YAML file per fingerprint, serialized
// across processes by an advisory lock on a shared ledger path.
type incidentFiles struct {
	dir string
}

func newIncidentFiles(dir string) *incidentFiles { return &incidentFiles{dir: dir} }

func (s *incidentFiles) Lock() (func() error, error) {
	return fsutil.LockFileWithin(filepath.Join(s.dir, "ledger"), incidentStoreLockTimeout)
}

func (s *incidentFiles) path(fp string) string { return filepath.Join(s.dir, fp+".yaml") }

func (s *incidentFiles) Load(fp string) (Incident, bool, error) {
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

func (s *incidentFiles) Save(in Incident) error {
	data, err := yaml.Marshal(in)
	if err != nil {
		return fmt.Errorf("incident store: encode: %w", err)
	}
	if err := fsutil.AtomicWriteMode(s.path(in.Fingerprint), data, 0o600); err != nil {
		return fmt.Errorf("incident store: write: %w", err)
	}
	return nil
}

func (s *incidentFiles) List() ([]Incident, error) {
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

var _ IncidentPersistence = (*incidentFiles)(nil)
