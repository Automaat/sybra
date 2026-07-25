package intervention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store is a project-partitioned, fingerprint-deduplicated, filesystem-backed
// record of intervention captures. Scrub-agnostic: callers must scrub
// work-derived fields on rec before Put.
type Store struct {
	dir string
	// mu serializes Put's read-modify-write so two concurrent captures of the
	// same fingerprint cannot both read Recurrences==N and clobber each other's
	// increment (a fingerprint deliberately excludes TaskID, so distinct tasks
	// unblocking with an identical blocker shape share a file).
	mu sync.Mutex
}

// New creates dir if it does not exist and returns a Store rooted there.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("intervention dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create intervention dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Put persists rec under projectKey, deduplicated by rec.Fingerprint.
//
// A fresh fingerprint is written as-is: Recurrences is stamped to 1,
// FirstSeen/LastSeen default to CreatedAt, and ReplayStatus defaults to
// ReplayStatusUnsupportedSimulation if unset.
//
// A fingerprint that already has a record on disk aggregates instead of
// duplicating: rec (the newest occurrence's details) is what gets persisted,
// but FirstSeen carries over from the existing record and Recurrences is the
// existing count plus one — so the file always reflects the latest instance
// of the failure while still tracking how many times it has recurred.
func (s *Store) Put(projectKey string, rec Record) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(rec.Fingerprint) == "" {
		return fmt.Errorf("intervention record has empty fingerprint")
	}
	projectDir, err := s.projectDir(projectKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return fmt.Errorf("create project intervention dir: %w", err)
	}
	path := filepath.Join(projectDir, fingerprintFileName(rec.Fingerprint))

	if rec.LastSeen.IsZero() {
		rec.LastSeen = rec.CreatedAt
	}

	// Serialize the read-modify-write below: the aggregate branch reads the
	// existing Recurrences and rewrites the whole file, so concurrent Puts on
	// the same fingerprint would otherwise lose an increment on a last-writer
	// clobber.
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, readErr := readRecord(path)
	switch {
	case readErr == nil:
		rec.FirstSeen = existing.FirstSeen
		if rec.FirstSeen.IsZero() {
			rec.FirstSeen = existing.CreatedAt
		}
		rec.Recurrences = existing.Recurrences + 1
		if rec.LastSeen.Before(existing.LastSeen) {
			rec.LastSeen = existing.LastSeen
		}
	case os.IsNotExist(readErr):
		rec.Recurrences = 1
		if rec.FirstSeen.IsZero() {
			rec.FirstSeen = rec.CreatedAt
		}
	default:
		return fmt.Errorf("read existing intervention record: %w", readErr)
	}
	if rec.ReplayStatus == "" {
		rec.ReplayStatus = ReplayStatusUnsupportedSimulation
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal intervention record: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write intervention record: %w", err)
	}
	return nil
}

// Query returns up to limit records for projectKey, most-recently-seen
// first.
func (s *Store) Query(projectKey string, limit int) ([]Record, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	projectDir, err := s.projectDir(projectKey)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project intervention dir: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		rec, err := readRecord(filepath.Join(projectDir, entry.Name()))
		if err != nil {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].LastSeen.Equal(records[j].LastSeen) {
			return records[i].LastSeen.After(records[j].LastSeen)
		}
		return records[i].Fingerprint < records[j].Fingerprint
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("parse intervention record: %w", err)
	}
	return rec, nil
}

func (s *Store) projectDir(projectKey string) (string, error) {
	safe, err := sanitizeProjectKey(projectKey)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, safe), nil
}

// sanitizeProjectKey validates projectKey is either an opaque work-project
// hash (see ProjectKey) or an "owner/repo" shape, then maps it to a
// filesystem-safe directory name. Mirrors
// internal/experience/store.go:sanitizeProjectID.
func sanitizeProjectKey(projectKey string) (string, error) {
	id := strings.TrimSpace(projectKey)
	if id == "" {
		return "", fmt.Errorf("project key is empty")
	}
	if isOpaqueWorkProjectKey(id) {
		return id, nil
	}
	if filepath.Clean(id) != id || strings.Contains(id, `\`) {
		return "", fmt.Errorf("invalid project key %q", projectKey)
	}
	owner, repo, ok := strings.Cut(id, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("invalid project key %q", projectKey)
	}
	if owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid project key %q", projectKey)
	}
	return "gh-" + hex.EncodeToString([]byte(owner)) + "-" + hex.EncodeToString([]byte(repo)), nil
}

func isOpaqueWorkProjectKey(id string) bool {
	const prefix = "work-"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+64 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// fingerprintFileName maps an arbitrary fingerprint string to a
// filesystem-safe, fixed-shape file name — a fingerprint is built from
// code-authored tokens (see Fingerprint) so it is already safe in practice,
// but hashing avoids re-deriving a character allowlist for every future field
// that might join it.
func fingerprintFileName(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:]) + ".json"
}
