package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
	// Limit caps matching records at the storage boundary. Zero is unlimited.
	Limit int
	// Strict refuses unreadable records instead of silently omitting coverage.
	Strict bool
	// MaxBytes bounds raw decoded input. Zero preserves the legacy read path.
	MaxBytes int
}

var ErrReadBudget = errors.New("audit input exceeds the report byte budget; narrow the window")

func Read(dir string, q Query) ([]Event, error) {
	files, err := auditFiles(dir, q.Since, q.Until)
	if err != nil {
		return nil, err
	}

	var events []Event
	remaining := q.MaxBytes
	for _, path := range files {
		fileQuery := q
		if q.Limit > 0 {
			fileQuery.Limit = q.Limit - len(events)
		}
		evts, err := readMatchingFile(path, fileQuery, &remaining)
		if err != nil {
			if q.Strict || q.MaxBytes > 0 {
				return nil, err
			}
			continue
		}
		events = append(events, evts...)
		if q.Limit > 0 && len(events) >= q.Limit {
			break
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

	// Compare in UTC: Log normalizes every event's timestamp to UTC and the
	// file is named from its date, while callers pass a window built from
	// time.Now(), which
	// carries the local zone. Formatting the bounds in local time makes the
	// filename window disagree with the writer for the hours where the two
	// dates differ — every query in that window silently returns nothing
	// rather than erroring.
	sinceDay := since.UTC().Format(time.DateOnly)
	untilDay := until.UTC().Format(time.DateOnly)

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

func readMatchingFile(path string, q Query, remaining *int) ([]Event, error) {
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
			if q.Strict {
				return nil, fmt.Errorf("audit contains an unreadable record: %w", err)
			}
			continue
		}
		if matchesQuery(e, q) {
			if q.MaxBytes > 0 {
				if len(scanner.Bytes()) > *remaining {
					return nil, ErrReadBudget
				}
				*remaining -= len(scanner.Bytes())
			}
			events = append(events, e)
		}
		if q.Limit > 0 && len(events) >= q.Limit {
			break
		}
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
