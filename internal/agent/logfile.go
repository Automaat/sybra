package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ParseLogFile reads an NDJSON agent log and returns up to maxEvents
// StreamEvents (the last N if the file exceeds the cap).
// Malformed lines are silently skipped.
func ParseLogFile(path string, maxEvents int) ([]StreamEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []StreamEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		var ev StreamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type == "" {
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// FindLogFile locates the NDJSON log for agentID inside logsDir/agents/ by
// globbing "{agentID}-*.ndjson". Returns the first match or an error.
func FindLogFile(logsDir, agentID string) (string, error) {
	pattern := filepath.Join(logsDir, "agents", agentID+"-*.ndjson")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no log file for agent %s", agentID)
	}
	return matches[0], nil
}
