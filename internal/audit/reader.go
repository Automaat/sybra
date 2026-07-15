package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Query struct {
	Since  time.Time
	Until  time.Time
	Type   string
	TaskID string
}

func Read(dir string, q Query) ([]Event, error) {
	files, err := auditFiles(dir, q.Since, q.Until)
	if err != nil {
		return nil, err
	}

	var events []Event
	for _, path := range files {
		evts, err := readFile(path)
		if err != nil {
			continue
		}
		for i := range evts {
			if matchesQuery(evts[i], q) {
				events = append(events, evts[i])
			}
		}
	}
	return events, nil
}

func auditFiles(dir string, since, until time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	sinceDay := since.Format(time.DateOnly)
	untilDay := until.Format(time.DateOnly)

	var paths []string
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".ndjson")
		if e.IsDir() || !ok {
			continue
		}
		if day >= sinceDay && day <= untilDay {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func readFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}

func matchesQuery(e Event, q Query) bool {
	if !q.Since.IsZero() && e.Timestamp.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && e.Timestamp.After(q.Until) {
		return false
	}
	if q.Type != "" && !strings.HasPrefix(e.Type, q.Type) {
		return false
	}
	if q.TaskID != "" && e.TaskID != q.TaskID {
		return false
	}
	return true
}
