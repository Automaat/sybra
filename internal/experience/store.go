package experience

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultMaxPerProject = 50

type Store struct {
	dir           string
	maxPerProject int
}

func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("experience dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create experience dir: %w", err)
	}
	return &Store{dir: dir, maxPerProject: defaultMaxPerProject}, nil
}

func (s *Store) Put(projectID string, rec Record) error {
	if s == nil {
		return nil
	}
	projectDir, err := s.projectDir(projectID)
	if err != nil {
		return err
	}
	recordID, err := sanitizeRecordID(rec.TaskID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return fmt.Errorf("create project experience dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal experience record: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(projectDir, recordID+".json"), data, 0o644); err != nil {
		return fmt.Errorf("write experience record: %w", err)
	}
	return s.enforceCap(projectDir)
}

func (s *Store) Query(projectID string, limit int) ([]Record, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	projectDir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project experience dir: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(projectDir, entry.Name()))
		if readErr != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	sortRecords(records)
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (s *Store) Delete(projectID string) error {
	if s == nil {
		return nil
	}
	projectDir, err := s.projectDir(projectID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(projectDir); err != nil {
		return fmt.Errorf("delete project experience dir: %w", err)
	}
	return nil
}

func (s *Store) projectDir(projectID string) (string, error) {
	safe, err := sanitizeProjectID(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, safe), nil
}

func (s *Store) enforceCap(projectDir string) error {
	maxRecords := s.maxPerProject
	if maxRecords <= 0 {
		maxRecords = defaultMaxPerProject
	}
	records, err := s.readProjectRecords(projectDir)
	if err != nil {
		return err
	}
	if len(records) <= maxRecords {
		return nil
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].record.CreatedAt.Equal(records[j].record.CreatedAt) {
			return records[i].record.CreatedAt.Before(records[j].record.CreatedAt)
		}
		return records[i].record.TaskID > records[j].record.TaskID
	})
	for i := range records[:len(records)-maxRecords] {
		if err := os.Remove(records[i].path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("evict experience record: %w", err)
		}
	}
	return nil
}

type storedRecord struct {
	record Record
	path   string
}

func (s *Store) readProjectRecords(projectDir string) ([]storedRecord, error) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project experience dir: %w", err)
	}
	records := make([]storedRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(projectDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		records = append(records, storedRecord{record: rec, path: path})
	}
	return records, nil
}

func sortRecords(records []Record) {
	sort.Slice(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.After(records[j].CreatedAt)
		}
		return records[i].TaskID < records[j].TaskID
	})
}

func sanitizeProjectID(projectID string) (string, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return "", fmt.Errorf("project id is empty")
	}
	if isOpaqueWorkProjectKey(id) {
		return id, nil
	}
	if filepath.Clean(id) != id || strings.Contains(id, `\`) {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	owner, repo, ok := strings.Cut(id, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("invalid project id %q", projectID)
	}
	if owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid project id %q", projectID)
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

func sanitizeRecordID(recordID string) (string, error) {
	id := strings.TrimSpace(recordID)
	if id == "" {
		return "", fmt.Errorf("record id is empty")
	}
	if filepath.Clean(id) != id || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid record id %q", recordID)
	}
	return id, nil
}
