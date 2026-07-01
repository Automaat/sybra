package learning

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/fsutil"
)

// defaultMaxDigests bounds the total number of persisted digests. Digests
// are periodic (hourly/daily at most), so this comfortably covers well over
// a year of history before the oldest entries start rolling off.
const defaultMaxDigests = 200

const latestFileName = "latest.json"

// Store is an append-only, local store of Digests under a single directory,
// one JSON file per digest named by its Key.Hash(). latest.json is a
// derived, disposable cache — correctness never depends on it; List always
// dir-scans the *.json files as the source of truth.
type Store struct {
	dir string
	max int
	mu  sync.Mutex
}

// New creates a Store rooted at dir, creating the directory if absent.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("learning: dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("learning: create dir: %w", err)
	}
	return &Store{dir: dir, max: defaultMaxDigests}, nil
}

// Put persists d if no digest already exists for d.Key() (dedup by source
// window/report digest). Returns stored=false without error when a digest
// for the same key already exists — this is the expected outcome of a
// repeated refresh, not a failure.
//
// The whole check-exists+write+cap sequence runs under mu so two concurrent
// Puts for the same key cannot both observe absence and both write.
func (s *Store) Put(d Digest) (stored bool, err error) {
	if s == nil {
		return false, nil
	}
	if d.Since.IsZero() || d.Until.IsZero() || strings.TrimSpace(d.ReportDigest) == "" {
		return false, fmt.Errorf("learning: since, until, and reportDigest are required")
	}
	if !d.Until.After(d.Since) {
		return false, fmt.Errorf("learning: until must be after since")
	}
	if len(d.Evidence) > MaxEvidenceRefs {
		d.Evidence = d.Evidence[:MaxEvidenceRefs]
	}

	hash := d.Key().Hash()
	path := filepath.Join(s.dir, hash+".json")

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, statErr := os.Stat(path); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("learning: stat %s: %w", path, statErr)
	}

	data, mErr := json.MarshalIndent(d, "", "  ")
	if mErr != nil {
		return false, fmt.Errorf("learning: marshal digest: %w", mErr)
	}
	if wErr := fsutil.AtomicWrite(path, data); wErr != nil {
		return false, fmt.Errorf("learning: write digest: %w", wErr)
	}

	digests, lErr := s.readAllLocked()
	if lErr != nil {
		slog.Warn("learning.put.rebuild-latest-err", "err", lErr)
		return true, nil
	}
	s.rebuildLatestLocked(digests)
	if capErr := s.enforceCapLocked(digests); capErr != nil {
		return true, capErr
	}
	return true, nil
}

// List returns every persisted digest, newest-first by GeneratedAt.
// Malformed rows are skipped and logged, never returned as an error — one
// corrupt file must not hide the rest of the journal.
func (s *Store) List() ([]Digest, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digests, err := s.readAllLocked()
	if err != nil {
		return nil, err
	}
	sortNewestFirst(digests)
	return digests, nil
}

// Get returns the digest stored for key, if any.
func (s *Store) Get(key Key) (Digest, bool, error) {
	if s == nil {
		return Digest{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOneLocked(key.Hash() + ".json")
}

// Latest returns the most recently generated digest, preferring the derived
// latest.json cache for a fast read but falling back to a full List scan
// when the cache is missing or fails to parse — correctness never depends
// on latest.json.
func (s *Store) Latest() (Digest, bool, error) {
	if s == nil {
		return Digest{}, false, nil
	}
	s.mu.Lock()
	d, ok, err := s.readOneLocked(latestFileName)
	s.mu.Unlock()
	if err == nil && ok {
		return d, true, nil
	}

	digests, lErr := s.List()
	if lErr != nil {
		return Digest{}, false, lErr
	}
	if len(digests) == 0 {
		return Digest{}, false, nil
	}
	return digests[0], true, nil
}

// readOneLocked reads and parses a single named file relative to s.dir.
// Caller must hold mu.
func (s *Store) readOneLocked(name string) (Digest, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return Digest{}, false, nil
		}
		return Digest{}, false, fmt.Errorf("learning: read %s: %w", name, err)
	}
	var d Digest
	if err := json.Unmarshal(data, &d); err != nil {
		return Digest{}, false, fmt.Errorf("learning: parse %s: %w", name, err)
	}
	return d, true, nil
}

// readAllLocked dir-scans s.dir for *.json digest files (excluding
// latest.json), parsing each. Malformed rows are skipped and logged, not
// returned as an error. Caller must hold mu.
func (s *Store) readAllLocked() ([]Digest, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("learning: readdir %s: %w", s.dir, err)
	}

	digests := make([]Digest, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == latestFileName {
			continue
		}
		data, rErr := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if rErr != nil {
			slog.Warn("learning.list.read-err", "file", e.Name(), "err", rErr)
			continue
		}
		var d Digest
		if jErr := json.Unmarshal(data, &d); jErr != nil {
			slog.Warn("learning.list.parse-err", "file", e.Name(), "err", jErr)
			continue
		}
		digests = append(digests, d)
	}
	return digests, nil
}

// rebuildLatestLocked writes latest.json from the newest digest in digests.
// Errors are logged, not returned — latest.json is a convenience cache, not
// the source of truth. Caller must hold mu.
func (s *Store) rebuildLatestLocked(digests []Digest) {
	if len(digests) == 0 {
		return
	}
	sorted := make([]Digest, len(digests))
	copy(sorted, digests)
	sortNewestFirst(sorted)

	data, err := json.MarshalIndent(sorted[0], "", "  ")
	if err != nil {
		slog.Warn("learning.latest.marshal-err", "err", err)
		return
	}
	if err := fsutil.AtomicWrite(filepath.Join(s.dir, latestFileName), data); err != nil {
		slog.Warn("learning.latest.write-err", "err", err)
	}
}

// enforceCapLocked evicts the oldest digests beyond s.max. Caller must hold mu.
func (s *Store) enforceCapLocked(digests []Digest) error {
	maxDigests := s.max
	if maxDigests <= 0 {
		maxDigests = defaultMaxDigests
	}
	if len(digests) <= maxDigests {
		return nil
	}
	sorted := make([]Digest, len(digests))
	copy(sorted, digests)
	sortNewestFirst(sorted)

	for i := range sorted[maxDigests:] {
		path := filepath.Join(s.dir, sorted[maxDigests+i].Key().Hash()+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("learning: evict digest: %w", err)
		}
	}
	return nil
}

// sortNewestFirst orders digests by GeneratedAt descending, breaking ties by
// hash for a deterministic order.
func sortNewestFirst(digests []Digest) {
	sort.Slice(digests, func(i, j int) bool {
		if !digests[i].GeneratedAt.Equal(digests[j].GeneratedAt) {
			return digests[i].GeneratedAt.After(digests[j].GeneratedAt)
		}
		return digests[i].Key().Hash() < digests[j].Key().Hash()
	})
}
